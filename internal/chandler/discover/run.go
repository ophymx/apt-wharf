// Package discover is the chandler discover orchestrator.
//
// One Run call:
//   - reads the parsed chandler.yaml,
//   - expands the matrix targets (or runs once with an empty target in
//     simple mode),
//   - fetches every referenced key over HTTPS and dearmors it,
//   - renders one deb822 .sources file per source[] entry,
//   - assembles a plan.Plan with one Package per matrix target,
//   - returns the plan for `cooper build -` to consume.
//
// This file is currently scaffolding: the run loop is sketched but
// the per-target work is stubbed. See chandler-design.md "Phases ·
// Discover" for the full algorithm.
package discover

import (
	"context"
	"errors"
	"time"

	"github.com/ophymx/apt-signpost/internal/chandler/config"
	"github.com/ophymx/apt-signpost/internal/chandler/keys"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// Options configures one Run invocation.
type Options struct {
	Tool   plan.Tool
	Now    func() time.Time
	Client *keys.Client // nil → default HTTP client
}

// Run is the chandler discover orchestrator. Walks the matrix (or the
// single simple-mode target), fetches keys, renders sources, and
// emits a plan.Plan compatible with `cooper build -`.
func Run(ctx context.Context, cfg *config.Config, opts Options) (*plan.Plan, error) {
	if cfg == nil {
		return nil, errors.New("discover: config is nil")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	out := &plan.Plan{
		SchemaVersion: plan.SchemaVersion,
		Tool:          opts.Tool,
		DiscoveredAt:  now().UTC().Format(time.RFC3339),
	}

	targets := cfg.Targets
	if len(targets) == 0 {
		// Simple mode: one synthetic empty target so the loop runs once.
		targets = []config.Target{{}}
	}

	for _, t := range targets {
		pkg := processTarget(ctx, cfg, t, opts)
		out.Packages = append(out.Packages, pkg)
	}
	return out, nil
}

// processTarget handles one matrix entry. Returns a plan.Package with
// Result=="ok" on success or Result=="error" with Error populated on
// any per-target failure.
//
// TODO: implement the full per-target pipeline:
//
//	1. template-substitute every templated field with target vars,
//	2. validate the resolved package name is unique across already-seen targets,
//	3. for each key slug referenced by any source[]: fetch, dearmor, log, record,
//	4. for each source: substitute templates, render the deb822 stanza,
//	5. if run_apt_update: include the postinst script in aux_files,
//	6. build the nfpm subtree (contents per keyring + per .sources file),
//	7. compute build_inputs_hash for the resolved BuildPlan,
//	8. return the plan.Package.
func processTarget(ctx context.Context, cfg *config.Config, t config.Target, opts Options) plan.Package {
	_ = ctx
	_ = cfg
	_ = t
	_ = opts
	return plan.Package{
		Name:   cfg.Package.Name,
		Result: plan.ResultError,
		Error: &plan.Error{
			Kind:    plan.ErrorKindDiscoveryFailed,
			Message: "chandler discover: per-target pipeline not yet implemented",
		},
	}
}
