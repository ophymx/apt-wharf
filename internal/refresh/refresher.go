package refresh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/ophymx/apt-signpost/internal/config"
	"github.com/ophymx/apt-signpost/internal/fetch"
	"github.com/ophymx/apt-signpost/internal/sign"
	"github.com/ophymx/apt-signpost/internal/source"
	"github.com/ophymx/apt-signpost/internal/store"
)

// Retention default per design-mvp.md: 30 minutes.
const Retention = 30 * time.Minute

// Refresher owns the periodic refresh loop and snapshot publication.
type Refresher struct {
	cfg         *config.Config
	signer      *sign.Signer
	store       *store.Store
	fetcher     *fetch.Fetcher
	httpClient  *http.Client
	discoverers map[string]source.Discoverer

	holder      *Holder
	tickMu      sync.Mutex // single writer; fires-while-held are dropped
	parallelism int

	log *slog.Logger
}

// Options controls construction. Discoverers must be keyed by source name;
// only sources with discoverers are processed (disabled or unknown sources
// are skipped).
type Options struct {
	Cfg         *config.Config
	Signer      *sign.Signer
	Store       *store.Store
	Fetcher     *fetch.Fetcher
	HTTPClient  *http.Client
	Discoverers map[string]source.Discoverer
	Holder      *Holder
	Parallelism int
	Logger      *slog.Logger
}

func New(opts Options) *Refresher {
	if opts.Holder == nil {
		opts.Holder = &Holder{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Parallelism <= 0 {
		opts.Parallelism = 4
	}
	return &Refresher{
		cfg:         opts.Cfg,
		signer:      opts.Signer,
		store:       opts.Store,
		fetcher:     opts.Fetcher,
		httpClient:  opts.HTTPClient,
		discoverers: opts.Discoverers,
		holder:      opts.Holder,
		parallelism: opts.Parallelism,
		log:         opts.Logger,
	}
}

func (r *Refresher) Snapshot() *Snapshot { return r.holder.Load() }

// SyncImportNew is the startup pass: any source declared in config that
// lacks a state file is fetched synchronously. Failure is fatal — the
// daemon should refuse to come up with a misconfigured new source.
func (r *Refresher) SyncImportNew(ctx context.Context) error {
	states, err := r.store.LoadSources()
	if err != nil {
		return fmt.Errorf("load sources: %w", err)
	}
	var missing []string
	for name, src := range r.cfg.Sources {
		if !src.IsEnabled() {
			continue
		}
		if _, ok := states[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	r.log.Info("syncImportNew", "count", len(missing), "sources", missing)
	for _, name := range missing {
		if err := r.processOne(ctx, name, nil); err != nil {
			return fmt.Errorf("source %s: %w", name, err)
		}
	}
	return nil
}

// Refresh runs one tick. Returns nil even if individual sources errored
// (those are isolated and logged); returns error only on tick-fatal cases
// like sign or build failure, where we keep the prior snapshot.
func (r *Refresher) Refresh(ctx context.Context) error {
	if !r.tickMu.TryLock() {
		r.log.Warn("refresh tick dropped: prior tick still holds the lock")
		return nil
	}
	defer r.tickMu.Unlock()

	prevStates, err := r.store.LoadSources()
	if err != nil {
		return fmt.Errorf("load sources: %w", err)
	}

	r.fanoutProcess(ctx, prevStates)

	// Reload after writes to capture all updates.
	states, err := r.store.LoadSources()
	if err != nil {
		return fmt.Errorf("reload sources: %w", err)
	}

	bootstrapState, debBytes, err := r.ensureBootstrap()
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	prev := r.holder.Load()
	snap, err := r.composeSnapshot(states, bootstrapState, debBytes, prev, time.Now())
	if err != nil {
		return fmt.Errorf("compose snapshot: %w", err)
	}
	r.holder.Store(snap)
	r.log.Info("refresh tick complete",
		"sources", len(states),
		"files", len(snap.Files),
		"redirects", len(snap.Redirects),
		"bootstrap_version", bootstrapState.Version)
	return nil
}

// Loop runs Refresh on a ticker with jitter until ctx is cancelled.
// Caller is expected to invoke Refresh once synchronously before Loop to
// build the initial snapshot.
func (r *Refresher) Loop(ctx context.Context) {
	interval := r.cfg.Refresh.Interval.AsDuration()
	jitter := r.cfg.Refresh.Jitter.AsDuration()
	src := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		wait := interval
		if jitter > 0 {
			wait += time.Duration(src.Int63n(int64(jitter)))
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if err := r.Refresh(ctx); err != nil {
			r.log.Error("refresh failed; prior snapshot retained", "err", err)
		}
	}
}

// fanoutProcess runs processOne for every enabled source in bounded parallel.
// Per-source errors are logged but do not abort the tick.
func (r *Refresher) fanoutProcess(ctx context.Context, prev map[string]*store.SourceState) {
	work := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < r.parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range work {
				if err := r.processOne(ctx, name, prev[name]); err != nil {
					r.log.Error("source failed; holding prior state",
						"source", name, "err", err)
				}
			}
		}()
	}
	for name, src := range r.cfg.Sources {
		if !src.IsEnabled() {
			continue
		}
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return
		case work <- name:
		}
	}
	close(work)
	wg.Wait()
}

// processOne probes the source, fetches if changed, parses control, and
// writes the per-source state file. Caller already holds the tick mutex.
func (r *Refresher) processOne(ctx context.Context, name string, prev *store.SourceState) error {
	disc, ok := r.discoverers[name]
	if !ok {
		return errors.New("no discoverer wired for this source")
	}

	var prevProbe *source.Probe
	if prev != nil {
		prevProbe = &source.Probe{
			URL:        prev.AssetURL,
			Token:      tokenFromState(prev),
			APIEtag:    prev.APIEtag,
			ReleaseTag: prev.ReleaseTag,
		}
	}

	res, err := disc.Probe(ctx, source.ProbeInput{Prev: prevProbe, HTTPClient: r.httpClient})
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	if res.Unchanged {
		// State file already current; just bump last_checked.
		if prev != nil {
			updated := *prev
			updated.LastChecked = time.Now().UTC()
			if err := r.store.WriteSource(&updated); err != nil {
				return fmt.Errorf("write last_checked: %w", err)
			}
		}
		return nil
	}

	needHash := res.Probe.AssetDigest == ""
	fr, err := r.fetcher.Fetch(ctx, fetch.Options{
		URL:      res.Probe.URL,
		NeedHash: needHash,
	})
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	sha := fr.SHA256
	if sha == "" {
		sha = res.Probe.AssetDigest
	}
	if sha == "" {
		return errors.New("no SHA256 available (digest missing and stream not hashed)")
	}
	if res.Probe.AssetDigest != "" && fr.SHA256 != "" && res.Probe.AssetDigest != fr.SHA256 {
		return fmt.Errorf("digest mismatch: github=%s computed=%s",
			res.Probe.AssetDigest, fr.SHA256)
	}

	now := time.Now().UTC()
	state := &store.SourceState{
		Name:           name,
		DiscoveryToken: res.Probe.Token,
		ReleaseTag:     res.Probe.ReleaseTag,
		AssetURL:       res.Probe.URL,
		AssetSize:      fr.Size,
		AssetSHA256:    sha,
		APIEtag:        res.Probe.APIEtag,
		Control:        string(fr.Control),
		LastChecked:    now,
		LastChanged:    now,
	}
	if rid := releaseIDFromToken(res.Probe.Token); rid != 0 {
		state.ReleaseID = rid
	}
	if err := r.store.WriteSource(state); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	r.log.Info("source updated",
		"source", name,
		"release_tag", state.ReleaseTag,
		"size", state.AssetSize,
		"sha256", state.AssetSHA256[:12]+"...",
		"hashed_locally", needHash)
	return nil
}

// tokenFromState reconstructs the Probe token from the persisted state.
// Prefers the opaque DiscoveryToken; falls back to the legacy ReleaseID for
// state files written before that field was introduced.
func tokenFromState(s *store.SourceState) string {
	if s.DiscoveryToken != "" {
		return s.DiscoveryToken
	}
	if s.ReleaseID == 0 {
		return ""
	}
	return fmt.Sprintf("%d", s.ReleaseID)
}

func releaseIDFromToken(token string) int64 {
	var n int64
	for _, c := range token {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
