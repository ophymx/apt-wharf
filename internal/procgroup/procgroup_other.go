//go:build !unix

// Package procgroup places a child process in its own process group on
// platforms that support one, so SIGKILL on context cancel takes the
// whole group rather than only the direct child. Shared by every
// apt-wharf tool that exec's helper binaries — signpost discovery,
// signpost external signing, cooper discovery — so they all get the
// same kill discipline without duplicating the platform shim.
package procgroup

import "os/exec"

// SetProcAttrs is a no-op on platforms without process groups; the default
// exec.CommandContext behavior (kill the direct child) is the best we get,
// with cmd.WaitDelay capping how long Wait can hang on dangling pipes.
func SetProcAttrs(_ *exec.Cmd) {}
