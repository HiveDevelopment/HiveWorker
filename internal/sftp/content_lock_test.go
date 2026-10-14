package sftp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExistingSFTPWriterHonorsNewInstallerLock(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "mod.jar"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := &lockedWriter{File: file, filesystem: &jailedFilesystem{root: root}}
	if _, err := writer.WriteAt([]byte("before"), 0); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, ".hivepanel"), 0750)
	os.WriteFile(filepath.Join(root, ".hivepanel", "lock.json"), []byte(`{"locked":true,"disable_files":true}`), 0640)
	if _, err := writer.WriteAt([]byte("after"), 0); err == nil {
		t.Fatal("open SFTP handle ignored installer lock")
	}
	data, _ := os.ReadFile(file.Name())
	if string(data) != "before" {
		t.Fatal("locked file changed")
	}
}
