package reprepro

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"al.essio.dev/pkg/shellescape"
)

// sshNotExistSentinel is the exit code the ssh readFile script uses
// to signal "remote path is absent." Picked so it doesn't collide
// with conventional shell exit codes (1=general, 2=misuse,
// 126/127=command issues, 130+=signals).
const sshNotExistSentinel = 42

// transport is the operations the reprepro backend performs against
// its target — local filesystem or a remote host over ssh/scp. The
// abstraction is narrow on purpose; only what drayman needs.
type transport interface {
	// runCmd runs name+args on the target, returning stdout bytes.
	// stderr is streamed to drayman's stderr. A non-zero exit code
	// is a Go error.
	runCmd(ctx context.Context, name string, args ...string) ([]byte, error)

	// readFile returns the contents of path on the target. Returns
	// (nil, fs.ErrNotExist) when the path is absent.
	readFile(ctx context.Context, path string) ([]byte, error)

	// listPackagesFiles returns absolute paths to every Packages
	// file under distsRoot on the target. Used by the reprepro
	// backend to scan all binary-<arch>/Packages files in the
	// configured distribution.
	listPackagesFiles(ctx context.Context, distsRoot string) ([]string, error)

	// stageFile makes localPath available to runCmd as the returned
	// path. For the local transport, returns localPath unchanged.
	// For ssh, scps localPath to a per-invocation temp path on the
	// remote host.
	stageFile(ctx context.Context, localPath string) (string, error)

	// removeFile deletes a path previously returned by stageFile.
	// No-op for the local transport (caller owns the local file).
	removeFile(ctx context.Context, path string) error
}

// localTransport runs reprepro on the same machine as drayman, against
// a local basedir. No copies are made; reprepro reads .debs directly
// from whatever path the caller provides.
type localTransport struct{}

func (localTransport) runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func (localTransport) readFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (localTransport) listPackagesFiles(_ context.Context, distsRoot string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(distsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A missing distsRoot is "no packages yet", not an error.
			if os.IsNotExist(err) {
				return fs.SkipAll
			}
			return err
		}
		if !d.IsDir() && d.Name() == "Packages" {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (localTransport) stageFile(_ context.Context, localPath string) (string, error) {
	return localPath, nil
}

func (localTransport) removeFile(_ context.Context, _ string) error {
	return nil
}

// sshTransport runs reprepro on a remote host over ssh/scp. dest is
// the ssh destination (user@host or alias from ~/.ssh/config). The
// user's ssh agent / authorized_keys must be configured for
// passwordless auth — drayman doesn't prompt.
type sshTransport struct {
	dest string
}

func (s sshTransport) runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	remote := shellescape.QuoteCommand(append([]string{name}, args...))
	cmd := exec.CommandContext(ctx, "ssh", s.dest, remote)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// readFile uses a shell wrapper that exits with sshNotExistSentinel
// when the remote path is absent, so the caller can distinguish "file
// not found" from "cat failed" (permissions, broken symlink, etc.)
// without relying on stdout being empty.
func (s sshTransport) readFile(ctx context.Context, path string) ([]byte, error) {
	q := shellescape.Quote(path)
	script := fmt.Sprintf("if [ -e %s ]; then cat -- %s; else exit %d; fi", q, q, sshNotExistSentinel)
	out, err := s.runCmd(ctx, "sh", "-c", script)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == sshNotExistSentinel {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	return out, nil
}

// listPackagesFiles uses the same sentinel pattern: a missing dists
// root exits sshNotExistSentinel and is mapped to an empty result;
// any other error bubbles up.
func (s sshTransport) listPackagesFiles(ctx context.Context, distsRoot string) ([]string, error) {
	q := shellescape.Quote(distsRoot)
	script := fmt.Sprintf("if [ ! -d %s ]; then exit %d; fi; find %s -name Packages -type f", q, sshNotExistSentinel, q)
	out, err := s.runCmd(ctx, "sh", "-c", script)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == sshNotExistSentinel {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

func (s sshTransport) stageFile(ctx context.Context, localPath string) (string, error) {
	id, err := randHex(4)
	if err != nil {
		return "", err
	}
	remote := fmt.Sprintf("/tmp/drayman-%s-%s", id, filepath.Base(localPath))
	cmd := exec.CommandContext(ctx, "scp", "-q", localPath, s.dest+":"+remote)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("scp %s → %s: %w", localPath, remote, err)
	}
	return remote, nil
}

func (s sshTransport) removeFile(ctx context.Context, path string) error {
	_, err := s.runCmd(ctx, "rm", "-f", path)
	return err
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
