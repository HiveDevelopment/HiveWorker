package cell

import (
	"errors"
	"fmt"
	"strings"

	"hivepanel-worker/internal/identity"
	hiveruntime "hivepanel-worker/internal/runtime"
)

func (m *Manager) Start(id string) error {
	m.mutex.Lock()

	cell, exists := m.cells[id]
	if !exists {
		m.mutex.Unlock()
		return errors.New("cell not found")
	}

	cellCopy := *cell

	selectedComb, err := m.resolveCellComb(cell)
	if err != nil {
		m.mutex.Unlock()
		return err
	}

	m.mutex.Unlock()

	if m.runtime.IsRunning(id) {
		return errors.New("cell already running")
	}

	variables := map[string]string{}

	for key, value := range selectedComb.Variables {
		variables[key] = value
	}

	for key, value := range cellCopy.Variables {
		variables[key] = value
	}

	variables["allocation.ip"] = cellCopy.Allocation.IP
	variables["allocation.port"] = fmt.Sprint(cellCopy.Allocation.Port)

	startupCommand := selectedComb.Startup
	if cellCopy.Startup.Command != "" {
		startupCommand = cellCopy.Startup.Command
	}

	image := selectedComb.Image
	if cellCopy.Docker.Image != "" {
		image = cellCopy.Docker.Image
	}

	command := renderTemplate(startupCommand, variables)
	environment := make(map[string]string, len(selectedComb.Environment))
	for key, value := range selectedComb.Environment {
		environment[key] = renderTemplate(value, variables)
	}

	if err := identity.Prepare(cellCopy.Dir); err != nil {
		return fmt.Errorf("prepare cell files: %w", err)
	}

	m.broadcastByID(id, "Cell started.")

	return m.runtime.Start(hiveruntime.RuntimeCell{
		ID:             cellCopy.ID,
		Command:        command,
		InstanceDir:    cellCopy.Dir,
		Image:          image,
		WorkingDir:     selectedComb.WorkingDir,
		Environment:    environment,
		AllocationIP:   cellCopy.Allocation.IP,
		AllocationPort: cellCopy.Allocation.Port,
		Limits:         cellCopy.Limits,
	}, func(line string) {
		m.broadcastByID(id, line)
	}, func() {
		m.broadcastByID(id, "Cell stopped.")
	})
}

func (m *Manager) Stop(id string) error {
	m.mutex.Lock()
	_, exists := m.cells[id]
	m.mutex.Unlock()

	if !exists {
		return errors.New("cell not found")
	}

	go func() {
		m.broadcastByID(id, "Stopping cell...")

		if err := m.runtime.Stop(id); err != nil {
			m.broadcastByID(id, "Stop failed: "+err.Error())
		}
	}()

	return nil
}

func (m *Manager) SendCommand(id string, command string) error {
	m.mutex.Lock()

	_, exists := m.cells[id]
	if !exists {
		m.mutex.Unlock()
		return errors.New("cell not found")
	}

	m.mutex.Unlock()

	m.broadcastByID(id, "> "+command)

	return m.runtime.SendCommand(id, command)
}

func (m *Manager) broadcastByID(id string, line string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	cell, exists := m.cells[id]
	if !exists {
		return
	}

	m.broadcast(cell, line)
}

func (m *Manager) IsRunning(id string) bool {
	id = strings.TrimSpace(id)

	if id == "" {
		return false
	}

	return m.runtime.IsRunning(id)
}
