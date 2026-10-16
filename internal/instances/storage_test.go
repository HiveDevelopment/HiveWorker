package instances

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyBudgetAndEscapingLinks(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	os.Mkdir(src, 0700)
	os.WriteFile(filepath.Join(src, "world"), []byte("hello"), 0600)
	dest := filepath.Join(root, "copy")
	if err := Copy(src, dest, 4); err == nil {
		t.Fatal("quota accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("failed copy retained")
	}
	if err := Copy(src, dest, 5); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dest, "world"))
	if string(data) != "hello" {
		t.Fatal("files lost")
	}
	os.Symlink("../outside", filepath.Join(src, "escape"))
	if err := Copy(src, filepath.Join(root, "unsafe"), 100); err == nil {
		t.Fatal("escaping link accepted")
	}
}
func TestContainedLinkIsNotFollowed(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	os.Mkdir(src, 0700)
	os.WriteFile(filepath.Join(src, "file"), []byte("hello"), 0600)
	os.Symlink("file", filepath.Join(src, "link"))
	size, err := Size(src)
	if err != nil || size != 5 {
		t.Fatalf("size %d %v", size, err)
	}
	dest := filepath.Join(root, "copy")
	if err = Copy(src, dest, 5); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(dest, "link"))
	if err != nil || link != "file" {
		t.Fatal("link changed")
	}
}
