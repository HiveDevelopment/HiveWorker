// Package instances implements bounded copies of Cell-owned files without following links.
package instances

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func Size(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, item fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		if item.Type().IsRegular() {
			info, err := item.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}
func Copy(source, destination string, budget int64) error {
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destination)
		}
	}()
	var copied int64
	err := filepath.WalkDir(source, func(path string, item fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(destination, rel)
		info, err := item.Info()
		if err != nil {
			return err
		}
		if item.IsDir() {
			return os.Mkdir(target, info.Mode().Perm()|0700)
		}
		if item.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(rel), link))
			if filepath.IsAbs(link) || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
				return errors.New("instance contains a symlink outside its files")
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return errors.New("instances support only regular files, folders and contained relative symlinks")
		}
		if budget >= 0 && info.Size() > budget-copied {
			return errors.New("saved instance would exceed the Cell disk allowance")
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()&0777)
		if err != nil {
			in.Close()
			return err
		}
		remaining := int64(-1)
		if budget >= 0 {
			remaining = budget - copied
		}
		reader := io.Reader(in)
		if remaining >= 0 {
			reader = io.LimitReader(in, remaining+1)
		}
		n, copyErr := io.Copy(out, reader)
		closeErr := out.Close()
		in.Close()
		copied += n
		if budget >= 0 && copied > budget {
			return errors.New("saved instance would exceed the Cell disk allowance")
		}
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	complete = true
	return nil
}
