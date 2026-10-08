//go:build !linux

package install

import "fmt"

func fileOwner(path string) (int, int, error) {
	return 0, 0, fmt.Errorf("file ownership inspection is only supported on Linux")
}
