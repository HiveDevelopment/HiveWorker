//go:build linux

package install

import (
	"os"
	"syscall"
)

func fileOwner(path string) (int, int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}

	stat := info.Sys().(*syscall.Stat_t)
	return int(stat.Uid), int(stat.Gid), nil
}
