package cell

import (
	"errors"
	"strings"

	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/comb"
	hiveruntime "hivepanel-worker/internal/runtime"
)

func (m *Manager) UpdateDefinition(id string, request UpdateCellDefinitionRequest) (*Cell, error) {
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	id = strings.TrimSpace(id)
	request.Name = strings.TrimSpace(request.Name)
	request.Comb = strings.TrimSpace(request.Comb)
	request.Docker.Image = strings.TrimSpace(request.Docker.Image)
	request.Startup.Command = strings.TrimSpace(request.Startup.Command)

	if id == "" {
		return nil, errors.New("cell id is required")
	}

	if request.Name == "" {
		return nil, errors.New("cell name is required")
	}

	if request.Comb == "" {
		return nil, errors.New("comb is required")
	}

	if request.CombData == nil {
		return nil, errors.New("comb data is required")
	}

	if _, err := comb.FromMap(request.CombData); err != nil {
		return nil, err
	}
	if request.Variables == nil {
		request.Variables = map[string]string{}
	}

	request.Allocation.IP = strings.TrimSpace(request.Allocation.IP)

	if request.Allocation.IP == "" || request.Allocation.Port <= 0 {
		return nil, errors.New("allocation is required")
	}

	additionalAllocations, err := normaliseAdditionalAllocations(
		request.Allocation,
		request.AdditionalAllocations,
	)
	if err != nil {
		return nil, err
	}

	ports := []int{request.Allocation.Port}
	for _, a := range additionalAllocations {
		ports = append(ports, a.Port)
	}
	if err := m.checkDefinitionPorts(id, ports); err != nil {
		return nil, err
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()

	gameCell, exists := m.cells[id]
	if !exists {
		return nil, errors.New("cell not found")
	}
	if gameCell.exclusiveOperation() && (gameCell.Lock.Type != "resource_operation" || gameCell.Lock.OperationToken != request.OperationToken) {
		return nil, errors.New("content installation is in progress")
	}

	if job := m.contentJobs[id]; job != nil && job.State == "complete" && request.ContentJobID != job.ID {
		return nil, errors.New("synchronize the completed content installation before changing the Cell definition")
	}
	previous := *gameCell
	running := m.runtime.IsRunning(id)
	oldAllocations := reservedCellAllocations(gameCell)
	if running && len(gameCell.ActiveAllocations) == 0 {
		gameCell.ActiveAllocations = cellAllocations(gameCell)
		if current, err := m.resolveCellComb(gameCell); err == nil {
			gameCell.ActiveStop = current.Stop
			if gameCell.ActiveStop == nil {
				gameCell.ActiveStop = &hiveruntime.StopOptions{Command: "stop", Timeout: 30, Signal: "SIGTERM"}
			}
		}
	}
	if !running {
		gameCell.ActiveAllocations = nil
		gameCell.ActiveStop = nil
	}
	gameCell.ConfigurationPending = running

	newAllocations := make([]allocation.Allocation, 0, 1+len(additionalAllocations))
	newAllocations = append(newAllocations, request.Allocation)
	newAllocations = append(newAllocations, additionalAllocations...)

	reserved := append(append([]allocation.Allocation(nil), newAllocations...), gameCell.ActiveAllocations...)
	if err := m.checkFirewall(id, reserved); err != nil {
		*gameCell = previous
		return nil, err
	}
	if err := m.allocManager.ReplaceCellReservations(oldAllocations, reserved); err != nil {
		*gameCell = previous
		return nil, err
	}

	gameCell.Name = request.Name
	gameCell.Comb = request.Comb
	gameCell.CombData = request.CombData
	gameCell.Variables = request.Variables
	gameCell.Allocation = request.Allocation
	gameCell.AdditionalAllocations = additionalAllocations
	gameCell.Limits = request.Limits
	gameCell.FeatureLimits = request.FeatureLimits
	gameCell.Docker = request.Docker
	gameCell.Startup = request.Startup

	if err := m.save(gameCell); err != nil {
		m.allocManager.ReplaceReservations(reserved, oldAllocations)
		*gameCell = previous
		return nil, err
	}

	return gameCell, nil
}

func cellAllocations(gameCell *Cell) []allocation.Allocation {
	allocations := make([]allocation.Allocation, 0, 1+len(gameCell.AdditionalAllocations))

	allocations = append(allocations, gameCell.Allocation)
	allocations = append(allocations, gameCell.AdditionalAllocations...)

	return allocations
}

func reservedCellAllocations(gameCell *Cell) []allocation.Allocation {
	return append(cellAllocations(gameCell), gameCell.ActiveAllocations...)
}

// Once the old runtime exits, removed ports may be reused even if the Cell has
// not been started again. Keep the desired definition pending for its next start.
func (m *Manager) releaseStoppedAllocations(id string) error {
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.mutex.Lock()
	defer m.mutex.Unlock()
	gameCell, exists := m.cells[id]
	if !exists || m.runtime.IsRunning(id) || len(gameCell.ActiveAllocations) == 0 {
		return nil
	}
	previous := *gameCell
	if err := m.checkFirewall(id, cellAllocations(gameCell)); err != nil {
		return err
	}
	gameCell.ActiveAllocations = nil
	gameCell.ActiveStop = nil
	if err := m.save(gameCell); err != nil {
		*gameCell = previous
		return err
	}
	m.allocManager.ReplaceReservations(reservedCellAllocations(&previous), cellAllocations(gameCell))
	return nil
}
