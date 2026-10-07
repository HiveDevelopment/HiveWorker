package cell

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/backup"
	"hivepanel-worker/internal/comb"
	hiveruntime "hivepanel-worker/internal/runtime"
)

func NewManager(
	dataDir string,
	instancesDir string,
	combManager *comb.Manager,
	runtime hiveruntime.Runtime,
	allocManager *allocation.Manager,
	backupManager *backup.Manager,
) *Manager {
	return &Manager{
		cells:           map[string]*Cell{},
		dataDir:         dataDir,
		instancesDir:    instancesDir,
		combManager:     combManager,
		runtime:         runtime,
		allocManager:    allocManager,
		backupManager:   backupManager,
		consoleSessions: map[string]ConsoleSession{},
	}
}

func (m *Manager) Load() error {
	if err := os.MkdirAll(m.cellDataDir(), 0755); err != nil {
		return err
	}

	if err := os.MkdirAll(m.instancesDir, 0755); err != nil {
		return err
	}

	files, err := os.ReadDir(m.cellDataDir())
	if err != nil {
		return err
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(m.cellDataDir(), file.Name()))
		if err != nil {
			continue
		}

		var gameCell Cell
		if err := json.Unmarshal(data, &gameCell); err != nil {
			continue
		}

		gameCell.console = []ConsoleEntry{}
		gameCell.subscribers = map[chan ConsoleEntry]bool{}

		if gameCell.AdditionalAllocations == nil {
			gameCell.AdditionalAllocations = []allocation.Allocation{}
		}

		if lock, err := loadLock(gameCell.Dir); err == nil {
			gameCell.Lock = lock
		}

		m.reserveCellAllocations(&gameCell)

		m.cells[gameCell.ID] = &gameCell
	}

	return nil
}

func (m *Manager) List() []*Cell {
	m.mutex.RLock()

	list := make([]*Cell, 0, len(m.cells))
	for _, gameCell := range m.cells {
		copyCell := *gameCell
		copyCell.AdditionalAllocations = append(
			[]allocation.Allocation(nil),
			gameCell.AdditionalAllocations...,
		)
		list = append(list, &copyCell)
	}

	m.mutex.RUnlock()

	// Keep runtime checks outside the manager lock.
	for _, gameCell := range list {
		if m.runtime.IsRunning(gameCell.ID) {
			gameCell.Status = "running"
		} else {
			gameCell.Status = "offline"
		}
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt < list[j].CreatedAt
	})

	return list
}

func (m *Manager) Get(id string) (*Cell, bool) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return nil, false
	}

	copyCell := *gameCell
	copyCell.AdditionalAllocations = append(
		[]allocation.Allocation(nil),
		gameCell.AdditionalAllocations...,
	)

	return &copyCell, true
}

func (m *Manager) Status(id string) (*Cell, error) {
	m.mutex.RLock()

	gameCell, exists := m.cells[id]
	if !exists {
		m.mutex.RUnlock()
		return nil, errors.New("cell not found")
	}

	copyCell := *gameCell
	copyCell.AdditionalAllocations = append(
		[]allocation.Allocation(nil),
		gameCell.AdditionalAllocations...,
	)

	m.mutex.RUnlock()

	// Keep this simple for now. Do not call Docker/runtime here.
	if copyCell.Status == "" {
		copyCell.Status = "offline"
	}

	return &copyCell, nil
}

func (m *Manager) reserveCellAllocations(gameCell *Cell) {
	m.allocManager.ReserveExisting(gameCell.Allocation)

	for _, allocation := range gameCell.AdditionalAllocations {
		m.allocManager.ReserveExisting(allocation)
	}
}
