//go:build !unix

package source

import "os/exec"

// setProcAttrs is a no-op on platforms without process groups; the default
// exec.CommandContext behavior (kill the direct child) is the best we get,
// with cmd.WaitDelay capping how long Wait can hang on dangling pipes.
func setProcAttrs(_ *exec.Cmd) {}
