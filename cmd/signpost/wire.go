package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ophymx/apt-signpost/internal/config"
	"github.com/ophymx/apt-signpost/internal/fetch"
	"github.com/ophymx/apt-signpost/internal/sign"
	"github.com/ophymx/apt-signpost/internal/source"
	"github.com/ophymx/apt-signpost/internal/store"
)

// Wired bundles the lazily constructed runtime objects shared by serve and
// check. No goroutines are started here; that's the caller's job.
type Wired struct {
	Cfg            *config.Config
	Signer         *sign.Signer
	Store          *store.Store
	Fetcher        *fetch.Fetcher
	Discoverers     map[string]source.Discoverer
	HTTPClient      *http.Client
	GlobalToken     *config.Secret            // may be nil
	PerSourceTokens map[string]*config.Secret // may be empty
	BucketRegistry  *source.Registry
}

// Wire reads secrets, loads the signing key, and builds discoverers. It does
// not touch state files or perform any network IO (other than the optional
// signing-key auto-generation when signing.auto_generate is set). The logger
// is forwarded to discoverers that emit diagnostics (currently the external
// type, which captures child stderr).
func Wire(cfg *config.Config, log *slog.Logger) (*Wired, error) {
	if log == nil {
		log = slog.Default()
	}
	pass, err := config.LoadSecret(cfg.Signing.PassphraseEnv, cfg.Signing.PassphraseFile,
		"signing.passphrase_env", "signing.passphrase_file")
	if err != nil {
		return nil, err
	}
	var passBytes []byte
	if pass != nil {
		passBytes = pass.Value
	}

	if cfg.Signing.AutoGenerate {
		uid := cfg.Bootstrap.Maintainer
		if uid == "" {
			uid = cfg.Repository.Origin + " APT signing key"
		}
		if err := sign.EnsureKey(cfg.Signing.KeyFile, uid); err != nil {
			return nil, fmt.Errorf("auto-generate signing key: %w", err)
		}
		// Re-run the secure-file check now that the file definitely exists,
		// so a hand-generated file with bad perms still fails loud.
		if err := config.CheckSecureFile(cfg.Signing.KeyFile, "signing.key_file"); err != nil {
			return nil, err
		}
	}

	signer, err := sign.Load(cfg.Signing.KeyFile, passBytes, cfg.Signing.NextPubkeyFile)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{Timeout: cfg.Refresh.HTTPTimeout.AsDuration()}
	if cfg.Refresh.HTTPTimeout.AsDuration() == 0 {
		httpClient.Timeout = 60 * time.Second
	}
	fetcher := fetch.New(httpClient)

	globalTok, err := config.LoadSecret(cfg.GitHub.TokenEnv, cfg.GitHub.TokenFile,
		"github.token_env", "github.token_file")
	if err != nil {
		return nil, err
	}

	registry := source.NewRegistry(cfg.GitHub.RateLimit.UnauthenticatedPerHour, cfg.GitHub.RateLimit.AuthenticatedPerHour)
	discoverers := map[string]source.Discoverer{}
	perSrcTokens := map[string]*config.Secret{}

	for name, src := range cfg.Sources {
		if !src.IsEnabled() {
			continue
		}
		switch src.Discovery.Type {
		case "github_release":
			tok, err := loadDiscoveryToken(name, src, globalTok)
			if err != nil {
				return nil, err
			}
			var tokBytes []byte
			if tok != nil {
				tokBytes = tok.Value
			}
			bucket := registry.BucketFor(tokBytes)
			bucketID := registry.CredentialID(tokBytes)
			client := source.NewGitHubClient(httpClient, tokBytes)
			d, err := source.NewGitHubReleaseDiscoverer(
				src.Discovery.Repo,
				src.Discovery.Asset,
				src.Discovery.IncludePrerelease,
				bucket,
				bucketID,
				client,
			)
			if err != nil {
				return nil, fmt.Errorf("source %s: %w", name, err)
			}
			discoverers[name] = d
			perSrcTokens[name] = tok
		case "latest_url":
			d, err := source.NewLatestURLDiscoverer(src.Discovery.URL, httpClient)
			if err != nil {
				return nil, fmt.Errorf("source %s: %w", name, err)
			}
			discoverers[name] = d
		case "external":
			d, err := source.NewExternalDiscoverer(
				src.Discovery.Command,
				src.Discovery.Timeout.AsDuration(),
				src.Discovery.Env,
				name,
				log,
			)
			if err != nil {
				return nil, fmt.Errorf("source %s: %w", name, err)
			}
			discoverers[name] = d
		default:
			// validateDiscovery already rejects unknown types; this is defensive.
			return nil, fmt.Errorf("source %s: unsupported discovery.type %q", name, src.Discovery.Type)
		}
	}

	st := store.New(cfg.Paths.StateDir)

	return &Wired{
		Cfg:             cfg,
		Signer:          signer,
		Store:           st,
		Fetcher:         fetcher,
		Discoverers:     discoverers,
		HTTPClient:      httpClient,
		GlobalToken:     globalTok,
		PerSourceTokens: perSrcTokens,
		BucketRegistry:  registry,
	}, nil
}

// loadDiscoveryToken resolves the per-source token, falling back to the global
// one when the source declares neither token_env nor token_file.
func loadDiscoveryToken(name string, src *config.Source, global *config.Secret) (*config.Secret, error) {
	if src.Discovery.TokenEnv == "" && src.Discovery.TokenFile == "" {
		return global, nil
	}
	return config.LoadSecret(src.Discovery.TokenEnv, src.Discovery.TokenFile,
		fmt.Sprintf("source %s discovery.token_env", name),
		fmt.Sprintf("source %s discovery.token_file", name))
}

// Zero best-effort wipes secret material owned by w.
func (w *Wired) Zero() {
	if w.Signer != nil {
		w.Signer.Zero()
	}
	if w.GlobalToken != nil {
		w.GlobalToken.Zero()
	}
	for _, t := range w.PerSourceTokens {
		if t != nil && t != w.GlobalToken {
			t.Zero()
		}
	}
}
