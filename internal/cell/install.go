package cell

import (
	"errors"
	"fmt"
	"os"

	"hivepanel-worker/internal/comb"
	"hivepanel-worker/internal/identity"
	"hivepanel-worker/internal/install"
)

func (m *Manager) StartInstall(id string) error {
	m.mutex.Lock()

	gameCell, exists := m.cells[id]
	if !exists {
		m.mutex.Unlock()
		return errors.New("cell not found")
	}

	if m.runtime.IsRunning(id) {
		m.mutex.Unlock()
		return errors.New("cell must be stopped before installing")
	}

	if gameCell.InstallStatus == "installing" {
		m.mutex.Unlock()
		return errors.New("cell installation is already running")
	}

	selectedComb, err := m.resolveCellComb(gameCell)
	if err != nil {
		m.mutex.Unlock()
		return err
	}

	gameCell.InstallStatus = "installing"
	gameCell.InstallError = ""
	_ = m.save(gameCell)
	m.broadcast(gameCell, "Install started.")

	dir := gameCell.Dir
	steps := append([]comb.InstallStep(nil), selectedComb.Install...)
	variables := make(map[string]string, len(gameCell.Variables))
	for key, value := range gameCell.Variables {
		variables[key] = value
	}

	m.mutex.Unlock()

	go m.runInstall(id, dir, variables, steps)
	return nil
}

func (m *Manager) Install(id string) error {
	if err := m.StartInstall(id); err != nil {
		return err
	}
	return nil
}

func (m *Manager) runInstall(id, dir string, variables map[string]string, steps []comb.InstallStep) {
	err := install.Run(
		dir,
		variables,
		steps,
		func(line string) {
			m.mutex.Lock()
			if gameCell, exists := m.cells[id]; exists {
				m.broadcast(gameCell, line)
			}
			m.mutex.Unlock()
		},
	)

	// Installer steps run on the host and may create root-owned files.
	// Repair ownership even if an install step failed.
	if ownershipErr := identity.Prepare(dir); ownershipErr != nil {
		if err == nil {
			err = fmt.Errorf("prepare installed files: %w", ownershipErr)
		}
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	if gameCell, exists := m.cells[id]; exists {
		if err != nil {
			gameCell.InstallStatus = "failed"
			gameCell.InstallError = err.Error()
			m.broadcast(gameCell, "Install failed: "+err.Error())
		} else {
			gameCell.InstallStatus = "installed"
			gameCell.InstallError = ""
			m.broadcast(gameCell, "Install completed.")
		}
		_ = m.save(gameCell)
	}
}

func (m *Manager) InstallState(id string) (string, string, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return "", "", errors.New("cell not found")
	}

	status := gameCell.InstallStatus
	if status == "" {
		status = "idle"
	}

	return status, gameCell.InstallError, nil
}

func (m *Manager) Reinstall(id string) error {
	m.mutex.Lock()

	gameCell, exists := m.cells[id]
	if !exists {
		m.mutex.Unlock()
		return errors.New("cell not found")
	}

	if m.runtime.IsRunning(id) {
		m.mutex.Unlock()
		return errors.New("cell must be stopped before reinstalling")
	}

	dir := gameCell.Dir

	m.broadcast(
		gameCell,
		"Preparing cell for reinstall.",
	)

	m.mutex.Unlock()

	if dir == "" {
		return errors.New("cell directory is empty")
	}

	info, err := os.Stat(dir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf(
			"stat cell directory: %w",
			err,
		)
	}

	if err == nil && !info.IsDir() {
		return errors.New("cell path is not a directory")
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf(
			"remove existing cell files: %w",
			err,
		)
	}

	if err := identity.Prepare(dir); err != nil {
		return fmt.Errorf(
			"recreate cell directory: %w",
			err,
		)
	}

	m.mutex.Lock()

	if gameCell, exists := m.cells[id]; exists {
		m.broadcast(
			gameCell,
			"Cell is ready for reinstall.",
		)
	}

	m.mutex.Unlock()

	return nil
}
