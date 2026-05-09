package config

import (
	"fmt"
	"os"
	"syscall"
)

// CheckSecureFile asserts the config-pointed file exists, has mode 0400, and
// is owned by the running process's effective UID. Used for any path that
// holds secret material (signing key, passphrase file, GitHub token file).
func CheckSecureFile(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %s: %w", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s %s is a directory, expected a regular file", label, path)
	}
	perm := info.Mode().Perm()
	if perm != 0o400 {
		return fmt.Errorf("%s %s has mode %#o, expected 0400", label, path, perm)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s %s: cannot read ownership (unsupported platform)", label, path)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s %s is owned by uid %d, expected service uid %d",
			label, path, stat.Uid, os.Geteuid())
	}
	return nil
}
