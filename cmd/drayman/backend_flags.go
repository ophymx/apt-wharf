package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	aptlyBackend "github.com/ophymx/apt-wharf/internal/drayman/backend/aptly"
	repreproBackend "github.com/ophymx/apt-wharf/internal/drayman/backend/reprepro"
)

// backendFlags carries the flag pointers shared by `peek` and
// `reconcile`. registerBackendFlags binds them to a FlagSet and
// resolveBackend turns them into a backend.Backend once parsed.
type backendFlags struct {
	kind *string

	// aptly
	aptlyURL      *string
	repo          *string
	publishPrefix *string
	publishDist   *string
	skipSigning   *bool

	// reprepro (local + ssh share most)
	repreproBasedir *string
	distribution    *string
	component       *string
	sshDest         *string
}

func registerBackendFlags(fs *flag.FlagSet) *backendFlags {
	bf := &backendFlags{
		kind:            fs.String("backend", "aptly", "repo backend: aptly | reprepro-local | reprepro-ssh"),
		aptlyURL:        fs.String("aptly-url", "", "(aptly) base URL of aptly API"),
		repo:            fs.String("repo", "", "(aptly) local repository name"),
		publishPrefix:   fs.String("publish-prefix", ".", "(aptly) publish prefix"),
		publishDist:     fs.String("publish-distribution", "", "(aptly) publish distribution"),
		skipSigning:     fs.Bool("skip-signing", false, "(aptly) set Signing.Skip on publish update"),
		repreproBasedir: fs.String("reprepro-basedir", "", "(reprepro) -b argument; path to repo basedir"),
		distribution:    fs.String("distribution", "", "(reprepro) apt distribution name"),
		component:       fs.String("component", "main", "(reprepro) apt component"),
		sshDest:         fs.String("ssh-dest", "", "(reprepro-ssh) ssh destination (user@host or ssh_config alias)"),
	}
	return bf
}

// resolveBackend constructs the appropriate Backend implementation
// from parsed flag values, or returns an error naming the missing
// required flags.
func (bf *backendFlags) resolveBackend() (backend.Backend, error) {
	switch *bf.kind {
	case "aptly":
		if *bf.aptlyURL == "" || *bf.repo == "" {
			return nil, errors.New("--backend aptly requires --aptly-url and --repo")
		}
		return aptlyBackend.New(aptlyBackend.Opts{
			BaseURL:       *bf.aptlyURL,
			HTTPClient:    &http.Client{Timeout: 5 * time.Minute},
			Repo:          *bf.repo,
			PublishPrefix: *bf.publishPrefix,
			Distribution:  *bf.publishDist,
			StagingDir:    "drayman-" + newRunID(),
			SkipSigning:   *bf.skipSigning,
		}), nil
	case "reprepro-local":
		if *bf.repreproBasedir == "" || *bf.distribution == "" {
			return nil, errors.New("--backend reprepro-local requires --reprepro-basedir and --distribution")
		}
		return repreproBackend.NewLocal(*bf.repreproBasedir, *bf.distribution, *bf.component), nil
	case "reprepro-ssh":
		if *bf.sshDest == "" || *bf.repreproBasedir == "" || *bf.distribution == "" {
			return nil, errors.New("--backend reprepro-ssh requires --ssh-dest, --reprepro-basedir, and --distribution")
		}
		return repreproBackend.NewSSH(*bf.sshDest, *bf.repreproBasedir, *bf.distribution, *bf.component), nil
	default:
		return nil, fmt.Errorf("unknown --backend %q (want: aptly, reprepro-local, reprepro-ssh)", *bf.kind)
	}
}

// publishRequiresDistribution returns true when the chosen backend's
// final Publish step needs a distribution argument (only aptly does;
// reprepro publishes atomically per Import).
func (bf *backendFlags) publishRequiresDistribution() bool {
	return *bf.kind == "aptly"
}

// newRunID returns 8 hex chars, suitable for namespacing aptly upload
// directories per drayman invocation. Collision-resistant enough that
// concurrent drayman runs don't tread on each other's staging.
func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
