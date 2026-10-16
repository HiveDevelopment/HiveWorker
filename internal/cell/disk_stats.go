package cell

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type diskMeasurement struct {
	dir     string
	bytes   int64
	ready   bool
	checked time.Time
	loading bool
}

func (m *Manager) cachedDisk(id, dir string) (int64, bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if m.diskCache == nil {
		m.diskCache = map[string]*diskMeasurement{}
		m.diskScans = make(chan struct{}, 2)
	}
	entry := m.diskCache[id]
	if entry == nil || entry.dir != dir {
		entry = &diskMeasurement{dir: dir}
		m.diskCache[id] = entry
	}
	if !entry.loading && time.Since(entry.checked) > 30*time.Second && dir != "" {
		select {
		case m.diskScans <- struct{}{}:
			entry.loading = true
			go func(entry *diskMeasurement) {
				defer func() { <-m.diskScans }()
				var size int64
				deadline := time.Now().Add(30 * time.Second)
				roots := []string{dir}
				if m.instancesDir != "" {
					roots = append(roots, m.instanceRoot(id))
				}
				var err error
				for _, scanRoot := range roots {
					err = filepath.WalkDir(scanRoot, func(path string, item fs.DirEntry, err error) error {
						if time.Now().After(deadline) {
							return os.ErrDeadlineExceeded
						}
						if errors.Is(err, os.ErrNotExist) && path == scanRoot {
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
							size += info.Size()
						}
						return nil
					})
					if err != nil {
						break
					}
				}
				m.mutex.Lock()
				defer m.mutex.Unlock()
				if m.diskCache[id] == entry {
					if err == nil {
						entry.bytes = size
						entry.ready = true
					}
					entry.loading = false
					entry.checked = time.Now()
				}
			}(entry)
		default:
		}
	}
	return entry.bytes, entry.ready
}
