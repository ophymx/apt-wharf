package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ophymx/apt-wharf/internal/config"
	"github.com/ophymx/apt-wharf/internal/refresh"
	"github.com/ophymx/apt-wharf/internal/server"
	"github.com/ophymx/apt-wharf/internal/status"
)

func cmdServe(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to YAML config")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", "json", "log format: json|text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log = newLogger(*logLevel, *logFormat)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	wired, err := Wire(cfg, log)
	if err != nil {
		return err
	}

	if err := wired.Store.EnsureDirs(); err != nil {
		wired.Zero()
		return fmt.Errorf("state dirs: %w", err)
	}

	holder := &refresh.Holder{}
	tracker := status.NewTracker()
	seedTracker(tracker, cfg, wired, log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Generation #0: build refresher, run synchronous import + initial
	// refresh, then start the periodic Loop. SIGHUP later swaps in new
	// generations without touching the listener.
	gen, err := startGeneration(ctx, cfg, wired, holder, tracker, log)
	if err != nil {
		wired.Zero()
		return err
	}
	if err := gen.refresher.SyncImportNew(ctx); err != nil {
		gen.stop()
		wired.Zero()
		return fmt.Errorf("sync import: %w", err)
	}
	if err := gen.refresher.Refresh(ctx); err != nil {
		gen.stop()
		wired.Zero()
		return fmt.Errorf("initial refresh: %w", err)
	}
	gen.startLoop()

	// daemon now owns gen + the secrets it carries; reload may swap.
	current := &daemonState{gen: gen, mu: &sync.Mutex{}}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           server.Handler(holder, tracker, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErrs := make(chan error, 1)
	go func() {
		log.Info("HTTP listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrs <- err
		}
		close(serveErrs)
	}()

	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	go reloadLoop(ctx, hupCh, current, *configPath, holder, tracker, log)

	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case err := <-serveErrs:
		if err != nil {
			cancel()
			current.gen.stop()
			current.gen.wired.Zero()
			return fmt.Errorf("http: %w", err)
		}
	}

	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer sCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	current.mu.Lock()
	current.gen.stop()
	current.gen.wired.Zero()
	current.mu.Unlock()
	return nil
}

// daemonState holds the current "generation" of reloadable state. The mutex
// serializes SIGHUP reloads against shutdown so no two generations are
// active at once.
type daemonState struct {
	mu  *sync.Mutex
	gen *generation
}

// generation bundles everything that gets replaced on a SIGHUP reload:
// the parsed config, the wired secrets+discoverers, the refresher, plus
// a per-generation cancellable context driving the Loop.
type generation struct {
	cfg       *config.Config
	wired     *Wired
	refresher *refresh.Refresher

	loopCtx    context.Context
	loopCancel context.CancelFunc
	loopDone   chan struct{} // closed exactly once: by the loop goroutine if started, by stop() otherwise
	started    atomic.Bool
	stopOnce   sync.Once
}

// startGeneration constructs a refresher and prepares the per-generation
// loop context. The loop is started lazily by startLoop so the caller can
// run the synchronous initial refresh before the ticker starts firing.
func startGeneration(parent context.Context, cfg *config.Config, wired *Wired, holder *refresh.Holder, tracker *status.Tracker, log *slog.Logger) (*generation, error) {
	rf := refresh.New(refresh.Options{
		Cfg:         cfg,
		Signer:      wired.Signer,
		Store:       wired.Store,
		Fetcher:     wired.Fetcher,
		HTTPClient:  wired.HTTPClient,
		Discoverers: wired.Discoverers,
		Tracker:     tracker,
		Holder:      holder,
		Logger:      log,
	})
	loopCtx, loopCancel := context.WithCancel(parent)
	return &generation{
		cfg:        cfg,
		wired:      wired,
		refresher:  rf,
		loopCtx:    loopCtx,
		loopCancel: loopCancel,
		loopDone:   make(chan struct{}),
	}, nil
}

// startLoop spawns the periodic refresh Loop on the generation's context.
// Call at most once per generation, after any synchronous priming refreshes.
func (g *generation) startLoop() {
	g.started.Store(true)
	go func() {
		defer close(g.loopDone)
		g.refresher.Loop(g.loopCtx)
	}()
}

// stop cancels the loop context and (when the loop has been started) waits
// for it to drain. Safe to call before startLoop — and idempotent — so the
// error paths in cmdServe and reload can call it without bookkeeping.
func (g *generation) stop() {
	g.stopOnce.Do(func() {
		g.loopCancel()
		if g.started.Load() {
			<-g.loopDone
		} else {
			close(g.loopDone)
		}
	})
}

// seedTracker populates tracker from the on-disk state and the source list
// in cfg. Errors loading state are logged and treated as "empty seed" — the
// next refresh tick will fill the tracker in. This runs both at startup
// and on every reload.
func seedTracker(tracker *status.Tracker, cfg *config.Config, wired *Wired, log *slog.Logger) {
	enabled := map[string]bool{}
	discoveryTypes := map[string]string{}
	for name, src := range cfg.Sources {
		enabled[name] = src.IsEnabled()
		discoveryTypes[name] = src.Discovery.Type
	}
	states, err := wired.Store.LoadSources()
	if err != nil {
		log.Warn("status seed: load sources failed", "err", err)
		states = nil
	}
	tracker.Seed(states, enabled, discoveryTypes)
	if bs, err := wired.Store.LoadBootstrap(); err == nil {
		tracker.SeedBootstrap(bs)
	}
}

// reloadLoop services SIGHUP signals until ctx is cancelled. Each signal
// triggers a config reload + immediate refresh. Reload failures keep the
// prior generation running.
func reloadLoop(ctx context.Context, hupCh <-chan os.Signal, current *daemonState, configPath string, holder *refresh.Holder, tracker *status.Tracker, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hupCh:
			if err := handleReload(ctx, current, configPath, holder, tracker, log); err != nil {
				log.Error("reload failed; keeping prior config", "err", err)
			}
		}
	}
}

// handleReload re-reads configPath, validates the result is hot-reloadable,
// builds a fresh Wired, swaps generations, and runs an immediate refresh.
// Any error is returned without mutating the current generation.
func handleReload(ctx context.Context, current *daemonState, configPath string, holder *refresh.Holder, tracker *status.Tracker, log *slog.Logger) error {
	log.Info("SIGHUP received; reloading config", "path", configPath)

	newCfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	current.mu.Lock()
	defer current.mu.Unlock()

	if err := assertHotReloadable(current.gen.cfg, newCfg); err != nil {
		return err
	}

	newWired, err := Wire(newCfg, log)
	if err != nil {
		return fmt.Errorf("wire: %w", err)
	}

	// New generation built successfully — commit. Stop the old loop, swap
	// in the new wired/refresher, then run the synchronous import +
	// refresh on the new generation before starting its loop.
	old := current.gen
	old.stop()

	seedTracker(tracker, newCfg, newWired, log)

	gen, err := startGeneration(ctx, newCfg, newWired, holder, tracker, log)
	if err != nil {
		// Roll back: restart old loop with old context. In practice this
		// only fires when ctx has already been cancelled, in which case
		// the daemon is shutting down anyway.
		newWired.Zero()
		return fmt.Errorf("restart generation: %w", err)
	}
	if err := gen.refresher.SyncImportNew(ctx); err != nil {
		log.Error("post-reload sync import failed", "err", err)
	}
	if err := gen.refresher.Refresh(ctx); err != nil {
		log.Error("post-reload refresh failed; serving prior snapshot", "err", err)
	}
	gen.startLoop()

	current.gen = gen
	old.wired.Zero()
	log.Info("config reloaded", "sources", len(newCfg.Sources))
	return nil
}
