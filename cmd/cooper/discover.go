package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"code.gitea.io/sdk/gitea"
	"github.com/google/go-github/v86/github"

	"github.com/ophymx/apt-wharf/internal/cli"
	"github.com/ophymx/apt-wharf/internal/cooper/config"
	"github.com/ophymx/apt-wharf/internal/cooper/discover"
	"github.com/ophymx/apt-wharf/internal/ghclient"
	"github.com/ophymx/apt-wharf/internal/giteaclient"
	"github.com/ophymx/apt-wharf/internal/secret"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

const cooperVersion = "0.1.0"

// stringSlice is the standard `--flag value` repeatable-string idiom.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// cmdDiscover implements `cooper discover <CONFIG>` per
// cooper-design.md §"CLI". Reads the top-level config, resolves the
// GitHub release per package, and emits a single plan.Plan JSON
// document on stdout (or -o OUTPUT).
func cmdDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: cooper discover <CONFIG> [--package NAME...] [-o OUTPUT]")
	}
	var pkgFilter stringSlice
	fs.Var(&pkgFilter, "package", "narrow to a package by name (repeatable)")
	output := fs.String("o", "-", "where to write the JSON plan (\"-\" for stdout)")

	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one positional CONFIG argument")
	}

	top, err := config.LoadTop(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("load top config: %w", err)
	}

	client, err := buildGitHubClient(top.GitHub)
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}

	gtFactory := newGiteaClientFactory()
	defer gtFactory.zero()

	opts := discover.Options{
		Tool: plan.Tool{
			Name:           "cooper",
			Version:        cooperVersion,
			FormatRevision: plan.FormatRevision,
		},
		Client:      client,
		GiteaClient: gtFactory.client,
	}
	if len(pkgFilter) > 0 {
		opts.PackageFilter = make(map[string]bool, len(pkgFilter))
		for _, n := range pkgFilter {
			opts.PackageFilter[n] = true
		}
	}

	doc, err := discover.Run(context.Background(), top, opts)
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}
	out = append(out, '\n')

	if *output == "-" {
		_, err = os.Stdout.Write(out)
	} else {
		err = os.WriteFile(*output, out, 0o644)
	}
	if err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	if anyError(doc) {
		return errors.New("one or more packages failed discovery")
	}
	return nil
}

// giteaClientFactory lazily constructs one gitea.Client per (server,
// resolved-token) and caches it for the duration of a discover run.
// Multiple recipes pointing at the same Gitea instance reuse the same
// client (and its underlying HTTP connection pool); recipes whose
// token specs resolve to the same plaintext token share a client even
// across different env-var/file paths. Tokens are zeroed on close.
type giteaClientFactory struct {
	mu      sync.Mutex
	clients map[string]*gitea.Client
	secrets []*secret.Secret // tracked for zero() at run end
}

func newGiteaClientFactory() *giteaClientFactory {
	return &giteaClientFactory{clients: map[string]*gitea.Client{}}
}

// client resolves the token spec, normalizes server URL, and returns
// the cached gitea.Client (constructing one on first miss).
func (f *giteaClientFactory) client(serverURL, tokenEnv, tokenFile string) (*gitea.Client, error) {
	server := strings.TrimRight(serverURL, "/")

	var tokenStr string
	if tokenEnv != "" || tokenFile != "" {
		tok, err := secret.LoadSecret(tokenEnv, tokenFile,
			"source.gitea.token_env", "source.gitea.token_file")
		if err != nil {
			return nil, err
		}
		if tok != nil {
			tokenStr = string(tok.Value)
			f.mu.Lock()
			f.secrets = append(f.secrets, tok)
			f.mu.Unlock()
		}
	}

	key := server + "\x00" + tokenStr
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.clients[key]; ok {
		return c, nil
	}
	c, err := giteaclient.New(http.DefaultClient, server, tokenStr)
	if err != nil {
		return nil, err
	}
	f.clients[key] = c
	return c, nil
}

func (f *giteaClientFactory) zero() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.secrets {
		s.Zero()
	}
}

// buildGitHubClient resolves the configured GitHub token (env or file)
// and wraps it in the shared ghclient transport. Empty token
// → unauthenticated client (subject to GitHub's anonymous rate limit).
func buildGitHubClient(gh config.GitHubConfig) (*github.Client, error) {
	tok, err := secret.LoadSecret(gh.TokenEnv, gh.TokenFile, "github.token_env", "github.token_file")
	if err != nil {
		return nil, err
	}
	var token []byte
	if tok != nil {
		token = tok.Value
	}
	return ghclient.New(http.DefaultClient, token), nil
}

func anyError(p *plan.Plan) bool {
	for _, pkg := range p.Packages {
		if pkg.Result == plan.ResultError {
			return true
		}
	}
	return false
}
