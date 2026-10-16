// Package filelease coordinates in-flight HTTP/SFTP I/O with instance directory swaps.
package filelease

import (
	"path/filepath"
	"sync"
)

var registry struct {
	sync.Mutex
	locks map[string]*sync.RWMutex
}

func lock(root string) *sync.RWMutex {
	registry.Lock()
	defer registry.Unlock()
	if registry.locks == nil {
		registry.locks = map[string]*sync.RWMutex{}
	}
	root = filepath.Clean(root)
	m := registry.locks[root]
	if m == nil {
		m = &sync.RWMutex{}
		registry.locks[root] = m
	}
	return m
}
func Read(root string) func()  { m := lock(root); m.RLock(); return m.RUnlock }
func Write(root string) func() { m := lock(root); m.Lock(); return m.Unlock }
