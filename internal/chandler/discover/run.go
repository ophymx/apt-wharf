// Package discover is the chandler discover orchestrator.
//
// One Run call:
//   - reads the parsed chandler.yaml,
//   - resolves source_date_epoch from the config file's git history,
//   - expands the matrix targets (or runs once in simple mode),
//   - fetches every referenced key over HTTPS and dearmors it,
//   - renders one deb822 .sources file per source[] entry,
//   - assembles a plan.Plan with one Package per matrix target,
//   - returns the plan for `cooper build -` to consume.
//
// See chandler-design.md "Phases · Discover" for the algorithm.
package discover

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/ophymx/apt-wharf/internal/chandler/config"
	"github.com/ophymx/apt-wharf/internal/chandler/keys"
	"github.com/ophymx/apt-wharf/internal/chandler/render"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// Chandler-specific error.kind values. The plan-design memo notes that
// exact strings are an implementation choice (see chandler-design.md
// "Discover JSON contract"); we keep them centralized here.
const (
	ErrorKindKeyFetchFailed   = "key_fetch_failed"
	ErrorKindKeyDearmorFailed = "key_dearmor_failed"
	ErrorKindTemplateFailed   = "template_render_failed"
)

// Options configures one Run invocation.
type Options struct {
	Tool plan.Tool
	Now  func() time.Time

	// Client overrides the HTTPS client used to fetch keys. Nil
	// means use a shared default; tests inject httptest clients.
	Client *keys.Client

	// Stderr receives the per-fetch UID/fingerprint/expiry log
	// lines. Nil means os.Stderr; tests inject bytes.Buffer.
	Stderr io.Writer

	// SkipGit makes Run synthesize a fixed source_date_epoch
	// rather than reading the config file's commit history.
	// Tests set this when running against config files that
	// aren't tracked in git. Production callers leave it false.
	SkipGit bool

	// AllowShallow bypasses the shallow-clone safety gate. Default
	// (false) causes Run to error out when the working tree is a
	// shallow git clone, because source_date_epoch derived from
	// `git log -- <configPath>` is unstable in that case. Set true
	// only when you genuinely intend to build from a shallow
	// checkout; SkipGit also implies the gate is skipped (tests).
	AllowShallow bool
}

// Run is the chandler discover orchestrator. Walks the matrix (or the
// single simple-mode target), fetches keys, renders sources, and emits
// a plan.Plan compatible with `cooper build -`.
func Run(ctx context.Context, cfg *config.Config, opts Options) (*plan.Plan, error) {
	if cfg == nil {
		return nil, errors.New("discover: config is nil")
	}
	if err := config.ValidateMatrix(cfg); err != nil {
		return nil, err
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Client == nil {
		opts.Client = &keys.Client{}
	}

	var (
		commitHash string
		commitTime time.Time
	)
	if opts.SkipGit {
		commitTime = time.Unix(0, 0).UTC()
	} else {
		// Shallow-clone guard. Fires before gitProvenance because the
		// error there is mechanically valid (git log returns SOMETHING)
		// but the resulting source_date_epoch silently destabilizes
		// build_inputs_hash.
		if !opts.AllowShallow {
			dir := configDirOrDot(cfg.Path)
			shallow, err := isShallowRepo(ctx, dir)
			if err != nil {
				return nil, fmt.Errorf("chandler: check shallow-clone status: %w", err)
			}
			if shallow {
				return nil, fmt.Errorf("chandler: shallow git clone detected; source_date_epoch derived from `git log -- %s` is unstable in shallow checkouts (HEAD's timestamp shadows the actual last-touch and forces drayman to bump the debian revision on every push). Fetch full history (e.g. fetch-depth: 0 in actions/checkout) or pass --allow-shallow to bypass", cfg.Path)
			}
		}
		c, t, err := gitProvenance(ctx, cfg.Path)
		if err != nil {
			return nil, fmt.Errorf("source_date_epoch unresolvable: %w", err)
		}
		commitHash, commitTime = c, t
	}

	out := &plan.Plan{
		SchemaVersion: plan.SchemaVersion,
		Tool:          opts.Tool,
		DiscoveredAt:  now().UTC().Format(time.RFC3339),
	}

	targets := cfg.Targets
	if len(targets) == 0 {
		targets = []config.Target{{}}
	}

	seenNames := make(map[string]bool, len(targets))
	for _, t := range targets {
		pkg := processTarget(ctx, cfg, t, commitHash, commitTime, opts)
		if pkg.Result == plan.ResultOK {
			if seenNames[pkg.Name] {
				pkg = errPkg(pkg.Name, plan.ErrorKindDiscoveryFailed,
					fmt.Errorf("resolved package.name %q collides with an earlier matrix target; template must reference a target-varying var", pkg.Name))
			} else {
				seenNames[pkg.Name] = true
			}
		}
		out.Packages = append(out.Packages, pkg)
	}
	return out, nil
}

// processTarget handles one matrix entry — the 8-step pipeline from
// chandler-design.md "Phases · Discover":
//
//  1. template-substitute every templated field with this target's vars,
//  2. (uniqueness check happens in the caller across targets),
//  3. for each key slug referenced by any source[]: fetch, dearmor, log, record,
//  4. for each source: substitute templates, render the deb822 stanza,
//  5. if run_apt_update: include the postinst script in aux_files,
//  6. build the nfpm subtree (contents per keyring + per .sources file),
//  7. compute build_inputs_hash for the resolved BuildPlan,
//  8. return the plan.Package.
func processTarget(ctx context.Context, cfg *config.Config, t config.Target,
	commitHash string, commitTime time.Time, opts Options) plan.Package {

	v := vars{Distro: t.Distro, Codename: t.Codename}

	// Step 1: resolve package name for this target.
	resolvedName, err := expand(cfg.Package.Name, v)
	if err != nil {
		return errPkg(cfg.Package.Name, plan.ErrorKindDiscoveryFailed,
			fmt.Errorf("expand package.name: %w", err))
	}

	// Step 3: fetch every referenced key (sorted for determinism).
	keySlugs := sortedReferencedKeys(cfg)
	auxFiles := make(map[string]plan.AuxFile)
	fetchedKeys := make([]plan.FetchedKey, 0, len(keySlugs))
	for _, slug := range keySlugs {
		k := cfg.Keys[slug]
		urls, err := resolveKeyURLs(k, v)
		if err != nil {
			return errPkg(resolvedName, plan.ErrorKindDiscoveryFailed,
				fmt.Errorf("expand keys.%s URL(s): %w", slug, err))
		}
		fetched, err := opts.Client.FetchURLs(ctx, urls)
		if err != nil {
			return errPkg(resolvedName, ErrorKindKeyFetchFailed,
				fmt.Errorf("fetch keys.%s: %w", slug, err))
		}
		fmt.Fprintf(opts.Stderr, "chandler: keys.%s fingerprint=%s uid=%q expiry=%s\n",
			slug, fetched.Info.Fingerprint, fetched.Info.UID, fetched.Info.Expiry)
		auxFiles["./"+slug+".gpg"] = plan.AuxFile{
			ContentB64: base64.StdEncoding.EncodeToString(fetched.Binary),
		}
		// Provenance captures the first URL when multiple are listed;
		// the slug-level summary records "what was fetched" not
		// "every URL individually."
		fetchedKeys = append(fetchedKeys, plan.FetchedKey{
			Slug:        slug,
			SourceURL:   urls[0],
			Fingerprint: fetched.Info.Fingerprint,
			UID:         fetched.Info.UID,
			Expiry:      fetched.Info.Expiry,
		})
	}

	// Step 4: render each source stanza into aux_files.
	for _, s := range cfg.Sources {
		uris, err := expandList(s.URIs, v)
		if err != nil {
			return errPkg(resolvedName, plan.ErrorKindDiscoveryFailed,
				fmt.Errorf("expand sources[%s].uris: %w", s.ID, err))
		}
		suites, err := expandList(s.Suites, v)
		if err != nil {
			return errPkg(resolvedName, plan.ErrorKindDiscoveryFailed,
				fmt.Errorf("expand sources[%s].suites: %w", s.ID, err))
		}
		components, err := expandList(s.Components, v)
		if err != nil {
			return errPkg(resolvedName, plan.ErrorKindDiscoveryFailed,
				fmt.Errorf("expand sources[%s].components: %w", s.ID, err))
		}
		archs, err := expandList(s.Architectures, v)
		if err != nil {
			return errPkg(resolvedName, plan.ErrorKindDiscoveryFailed,
				fmt.Errorf("expand sources[%s].architectures: %w", s.ID, err))
		}
		types, err := expandList(s.Types, v)
		if err != nil {
			return errPkg(resolvedName, plan.ErrorKindDiscoveryFailed,
				fmt.Errorf("expand sources[%s].types: %w", s.ID, err))
		}
		body, err := render.Stanza(render.SourceStanza{
			Types:         types,
			URIs:          uris,
			Suites:        suites,
			Components:    components,
			Architectures: archs,
			SignedBy:      "/usr/share/keyrings/" + s.Key + ".gpg",
		})
		if err != nil {
			return errPkg(resolvedName, plan.ErrorKindDiscoveryFailed,
				fmt.Errorf("render sources[%s]: %w", s.ID, err))
		}
		auxFiles["./"+s.ID+".sources"] = plan.AuxFile{
			ContentB64: base64.StdEncoding.EncodeToString([]byte(body)),
		}
	}

	// Step 5: optional postinst.
	if cfg.Package.RunAptUpdate {
		auxFiles["./postinst.sh"] = plan.AuxFile{
			ContentB64: base64.StdEncoding.EncodeToString([]byte(render.Postinst())),
		}
	}

	// Step 6: build the nfpm subtree.
	sourceIDs := sortedSourceIDs(cfg)
	nfpmJSON, err := buildNfpm(cfg, resolvedName, keySlugs, sourceIDs)
	if err != nil {
		return errPkg(resolvedName, plan.ErrorKindDiscoveryFailed, err)
	}

	// Step 7: compute build_inputs_hash.
	bp := plan.BuildPlan{
		SourceDateEpoch: commitTime.Unix(),
		Nfpm:            nfpmJSON,
		AuxFiles:        auxFiles,
	}
	hash, err := plan.ComputeBuildInputsHash(opts.Tool.FormatRevision, nil, bp)
	if err != nil {
		return errPkg(resolvedName, plan.ErrorKindDiscoveryFailed,
			fmt.Errorf("compute build_inputs_hash: %w", err))
	}

	// Step 8: assemble the plan.Package.
	source := &plan.Source{
		Kind:        plan.SourceKindChandler,
		ConfigPath:  cfg.Path,
		GitCommit:   commitHash,
		GitDate:     commitTime.UTC().Format(time.RFC3339),
		Target:      &plan.Target{Distro: t.Distro, Codename: t.Codename},
		FetchedKeys: fetchedKeys,
	}
	artifact := plan.Artifact{
		Arch:   "all",
		Assets: nil,
		Deb: plan.Deb{
			Filename:        fmt.Sprintf("%s_%s_all.deb", resolvedName, cfg.Package.Version),
			BuildInputsHash: hash,
		},
		BuildPlan: bp,
	}
	return plan.Package{
		Name:      resolvedName,
		Result:    plan.ResultOK,
		Source:    source,
		Artifacts: []plan.Artifact{artifact},
	}
}

// resolveKeyURLs returns the URL list for one Key, expanded per target.
// Whether the YAML used `url:` or `urls:`, the result is always a slice
// FetchURLs can consume.
func resolveKeyURLs(k config.Key, v vars) ([]string, error) {
	if k.URL != "" {
		u, err := expand(k.URL, v)
		if err != nil {
			return nil, err
		}
		return []string{u}, nil
	}
	return expandList(k.URLs, v)
}

// sortedReferencedKeys returns the set of key slugs actually used by
// at least one source[], in lex order. ValidateMatrix has already
// rejected unused keys, so this is the same as sorting Keys keys, but
// going through sources[] explicitly documents the dependency.
func sortedReferencedKeys(cfg *config.Config) []string {
	used := make(map[string]bool, len(cfg.Keys))
	for _, s := range cfg.Sources {
		used[s.Key] = true
	}
	out := make([]string, 0, len(used))
	for slug := range used {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

func sortedSourceIDs(cfg *config.Config) []string {
	out := make([]string, len(cfg.Sources))
	for i, s := range cfg.Sources {
		out[i] = s.ID
	}
	sort.Strings(out)
	return out
}

func errPkg(name, kind string, err error) plan.Package {
	return plan.Package{
		Name:   name,
		Result: plan.ResultError,
		Error: &plan.Error{
			Kind:    kind,
			Message: err.Error(),
		},
	}
}
