//go:build !linux

package identity

import "fmt"

func Ensure() (string, int, int, error) {
	return "", 0, 0, fmt.Errorf("managed game container identity requires Linux")
}

func Prepare(root string) error {
	return fmt.Errorf("managed game container identity requires Linux")
}
