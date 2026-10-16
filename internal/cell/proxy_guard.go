package cell

import (
	"errors"
	"hivepanel-worker/internal/allocation"
)

func (m *Manager) SetDefinitionGuard(guard func(string, []int) error) {
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.definitionGuard = guard
}
func (m *Manager) WithDefinitionLock(callback func() error) error {
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	return callback()
}
func (m *Manager) WithCellDefinition(id string, callback func([]int) error) error {
	return m.WithDefinitionLock(func() error {
		m.mutex.RLock()
		c, ok := m.cells[id]
		if !ok {
			m.mutex.RUnlock()
			return errors.New("Cell not found")
		}
		ports := []int{}
		for _, a := range cellAllocations(c) {
			ports = append(ports, a.Port)
		}
		m.mutex.RUnlock()
		return callback(ports)
	})
}
func (m *Manager) checkDefinitionPorts(id string, ports []int) error {
	if m.definitionGuard != nil {
		return m.definitionGuard(id, ports)
	}
	return nil
}

func (m *Manager) SetFirewallGuard(guard func(string, []allocation.Allocation) error) {
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.firewallGuard = guard
}
func (m *Manager) checkFirewall(id string, allocations []allocation.Allocation) error {
	if m.firewallGuard != nil {
		return m.firewallGuard(id, allocations)
	}
	return nil
}
