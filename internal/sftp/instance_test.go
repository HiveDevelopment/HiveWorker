package sftp

import (
	"hivepanel-worker/internal/panel"
	"os"
	"path/filepath"
	"testing"
)

func TestOldSFTPSessionCannotWriteAfterInstanceActivation(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	os.Mkdir(active, 0700)
	info, err := os.Stat(active)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := &jailedFilesystem{root: active, rootIdentity: info, permissions: panel.SFTPPermissions{Write: true}}
	os.Rename(active, filepath.Join(root, "saved"))
	os.Mkdir(active, 0700)
	if err = filesystem.ensureCurrentInstance(); err == nil {
		t.Fatal("old session retained write access after the active directory changed")
	}
}
