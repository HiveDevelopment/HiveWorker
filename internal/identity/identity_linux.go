//go:build linux

package identity

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

const account = "hiveworker-cell"

var accountMutex sync.Mutex

// Ensure returns the dedicated, host-managed identity for game containers.
// The operating system allocates the UID/GID; no numeric ID is hardcoded.
func Ensure() (string, int, int, error) {
	accountMutex.Lock()
	defer accountMutex.Unlock()
	current, err := user.Lookup(account)
	if err != nil {
		if os.Geteuid() != 0 {
			return "", 0, 0, fmt.Errorf("managed account %q missing and worker is not root: %w", account, err)
		}
		output, createErr := exec.Command("useradd", "--system", "--no-create-home", "--shell", "/sbin/nologin", "--user-group", account).CombinedOutput()
		if createErr != nil {
			// Another process may have created the account concurrently.
			if _, lookupErr := user.Lookup(account); lookupErr != nil {
				return "", 0, 0, fmt.Errorf("create managed account: %w: %s", createErr, output)
			}
		}
		current, err = user.Lookup(account)
		if err != nil {
			return "", 0, 0, fmt.Errorf("lookup managed account: %w", err)
		}
	}
	uid, err := strconv.Atoi(current.Uid)
	if err != nil {
		return "", 0, 0, err
	}
	gid, err := strconv.Atoi(current.Gid)
	if err != nil {
		return "", 0, 0, err
	}
	return fmt.Sprintf("%d:%d", uid, gid), uid, gid, nil
}

// Prepare fixes ownership of a Cell tree, without following symlinks.
// Must only be called for a trusted Cell root, never a user-supplied path.
func Prepare(root string) error {
	_, uid, gid, err := Ensure()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0750); err != nil {
		return err
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot stat %s", path)
		}
		if int(stat.Uid) == uid && int(stat.Gid) == gid {
			return nil
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", path, err)
		}
		return nil
	})
}
