package cell

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const hivepanelDir = ".hivepanel"
const lockFileName = "lock.json"

func DefaultLock(lockType string, reason string, message string) CellLock {
	lock := CellLock{
		Locked:  true,
		Type:    lockType,
		Reason:  reason,
		Message: message,
	}

	switch lockType {
	case "importing":
		lock.DisablePower = true
		lock.DisableFiles = true
		lock.DisableBackups = true
		lock.DisableSettings = true
		lock.DisableImporter = true

	case "restoring_backup":
		lock.DisablePower = true
		lock.DisableConsole = true
		lock.DisableFiles = true
		lock.DisableBackups = true
		lock.DisableSettings = true
		lock.DisableImporter = true

	case "creating_backup":
		lock.DisableBackups = true
		lock.DisableImporter = true

	case "suspended", "billing_hold":
		lock.DisablePower = true
		lock.DisableConsole = true
		lock.DisableFiles = true
		lock.DisableBackups = true
		lock.DisableSettings = true
		lock.DisableImporter = true

	case "maintenance":
		lock.DisablePower = true
		lock.DisableImporter = true

	case "admin_lock":
		lock.DisablePower = true
		lock.DisableConsole = true
		lock.DisableFiles = true
		lock.DisableBackups = true
		lock.DisableSettings = true
		lock.DisableImporter = true

	default:
		lock.DisablePower = true
		lock.DisableFiles = true
		lock.DisableBackups = true
		lock.DisableSettings = true
		lock.DisableImporter = true
	}

	return lock
}

func (m *Manager) LockCell(id string, lockType string, reason string, message string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return errors.New("cell not found")
	}

	gameCell.Lock = DefaultLock(lockType, reason, message)

	return saveLock(gameCell.Dir, gameCell.Lock)
}

func (m *Manager) UnlockCell(id string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return errors.New("cell not found")
	}
	if gameCell.Lock.Type == "content_install" {
		return errors.New("content installation must finish before unlocking")
	}

	gameCell.Lock = CellLock{}

	return deleteLock(gameCell.Dir)
}

func (m *Manager) CellLock(id string) (CellLock, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return CellLock{}, errors.New("cell not found")
	}

	return gameCell.Lock, nil
}

func (m *Manager) IsLocked(id string) bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return false
	}

	return gameCell.Lock.Locked
}

func (m *Manager) EnsureNotLocked(id string) error {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return errors.New("cell not found")
	}

	if gameCell.Lock.Locked {
		if gameCell.Lock.Message != "" {
			return errors.New(gameCell.Lock.Message)
		}

		return errors.New("cell is locked")
	}

	return nil
}

func (m *Manager) EnsurePowerAllowed(id string) error {
	return m.ensureCapabilityAllowed(id, func(lock CellLock) bool {
		return lock.DisablePower
	}, "power controls are disabled while this server is locked")
}

func (m *Manager) EnsureConsoleAllowed(id string) error {
	return m.ensureCapabilityAllowed(id, func(lock CellLock) bool {
		return lock.DisableConsole
	}, "console access is disabled while this server is locked")
}

func (m *Manager) EnsureFilesAllowed(id string) error {
	return m.ensureCapabilityAllowed(id, func(lock CellLock) bool {
		return lock.DisableFiles
	}, "file access is disabled while this server is locked")
}

func (m *Manager) EnsureBackupsAllowed(id string) error {
	return m.ensureCapabilityAllowed(id, func(lock CellLock) bool {
		return lock.DisableBackups
	}, "backup actions are disabled while this server is locked")
}

func (m *Manager) EnsureSettingsAllowed(id string) error {
	return m.ensureCapabilityAllowed(id, func(lock CellLock) bool {
		return lock.DisableSettings
	}, "settings are disabled while this server is locked")
}

func (m *Manager) EnsureImporterAllowed(id string) error {
	return m.ensureCapabilityAllowed(id, func(lock CellLock) bool {
		return lock.DisableImporter
	}, "server importer is disabled while this server is locked")
}

func (m *Manager) ensureCapabilityAllowed(id string, disabled func(CellLock) bool, fallback string) error {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return errors.New("cell not found")
	}

	if !gameCell.Lock.Locked {
		return nil
	}

	if disabled(gameCell.Lock) {
		if gameCell.Lock.Message != "" {
			return errors.New(gameCell.Lock.Message)
		}

		return errors.New(fallback)
	}

	return nil
}

func (m *Manager) LoadCellLock(id string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return errors.New("cell not found")
	}

	lock, err := loadLock(gameCell.Dir)
	if err != nil {
		return err
	}

	gameCell.Lock = lock

	return nil
}

func saveLock(cellDir string, lock CellLock) error {
	dir := filepath.Join(cellDir, hivepanelDir)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, ".lock-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, lockFileName))
}

func loadLock(cellDir string) (CellLock, error) {
	data, err := os.ReadFile(filepath.Join(cellDir, hivepanelDir, lockFileName))
	if err != nil {
		return CellLock{}, err
	}

	var lock CellLock

	if err := json.Unmarshal(data, &lock); err != nil {
		return CellLock{}, err
	}

	return lock, nil
}

func deleteLock(cellDir string) error {
	path := filepath.Join(cellDir, hivepanelDir, lockFileName)

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}
