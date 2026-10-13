package cell

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskMeasurementRunsInBackgroundAndDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("not counted"), 0600)
	_ = os.Symlink(outside, filepath.Join(dir, "outside"))
	manager := &Manager{}
	if _, ready := manager.cachedDisk("cell", dir); ready {
		t.Fatal("scan blocked request")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		size, ready := manager.cachedDisk("cell", dir)
		if ready {
			if size != 5 {
				t.Fatalf("unexpected bytes: %d", size)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scan did not complete")
}
