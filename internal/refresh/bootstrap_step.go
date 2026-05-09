package refresh

import (
	"fmt"
	"time"

	"github.com/ophymx/apt-signpost/internal/bootstrap"
	"github.com/ophymx/apt-signpost/internal/store"
)

// ensureBootstrap returns the active BootstrapState plus the cached .deb
// bytes. If the input hash differs from the persisted state (or no state
// exists), rebuilds via nfpm and writes the new state + .deb to disk.
//
// Logs the rebuild reason at INFO so operators can see "why did the
// version bump?" without grepping git history. Common reasons: cold start
// (no prev), config edit (input_hash changed), cached .deb deleted from
// disk.
func (r *Refresher) ensureBootstrap() (*store.BootstrapState, []byte, error) {
	in := &bootstrap.Inputs{
		PackageName:   r.cfg.Bootstrap.PackageName,
		Maintainer:    r.cfg.Bootstrap.Maintainer,
		Description:   r.cfg.Bootstrap.Description,
		BaseURL:       r.cfg.Repository.BaseURL,
		Codename:      r.cfg.Suite.Codename,
		Components:    []string{"main"},
		Architectures: r.cfg.Suite.Architectures,
		KeyringBytes:  r.signer.KeyringBytes(),
	}
	wantHash := bootstrap.InputHash(in)

	prev, err := r.store.LoadBootstrap()
	if err != nil {
		return nil, nil, fmt.Errorf("load bootstrap state: %w", err)
	}

	reason := "cold start (no prev state)"
	if prev != nil {
		switch {
		case prev.InputHash != wantHash:
			reason = fmt.Sprintf("input hash changed (prev=%s want=%s)",
				prev.InputHash, wantHash)
		default:
			body, err := r.store.LoadBootstrapDeb(prev.Version)
			if err != nil {
				reason = fmt.Sprintf("cached .deb missing (%v)", err)
				break
			}
			r.log.Info("bootstrap reused", "version", prev.Version,
				"input_hash", prev.InputHash)
			return prev, body, nil
		}
	}

	prevVer := ""
	if prev != nil {
		prevVer = prev.Version
	}
	newVersion := bootstrap.NextVersion(prevVer, time.Now())
	body, sha, err := bootstrap.Build(in, newVersion)
	if err != nil {
		return nil, nil, fmt.Errorf("build bootstrap deb: %w", err)
	}
	if err := r.store.WriteBootstrapDeb(newVersion, body); err != nil {
		return nil, nil, fmt.Errorf("write bootstrap deb: %w", err)
	}
	state := &store.BootstrapState{
		Version:   newVersion,
		InputHash: wantHash,
		Filename:  bootstrap.Filename(in.PackageName, newVersion),
		Size:      int64(len(body)),
		SHA256:    sha,
	}
	if err := r.store.WriteBootstrap(state); err != nil {
		return nil, nil, fmt.Errorf("write bootstrap state: %w", err)
	}
	r.log.Info("bootstrap rebuilt",
		"version", newVersion,
		"prev_version", prevVer,
		"reason", reason,
		"size", state.Size,
		"input_hash", wantHash)
	return state, body, nil
}
