package cell

import (
	"errors"
	"github.com/google/uuid"
)

func (m *Manager) BeginOperation(id, token string) error {
	if _, err := uuid.Parse(token); err != nil {
		return errors.New("invalid operation token")
	}
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.mutex.Lock()
	defer m.mutex.Unlock()
	c, ok := m.cells[id]
	if !ok {
		return errors.New("Cell not found")
	}
	if c.Lock.Type == "resource_operation" && c.Lock.OperationToken == token {
		return nil
	}
	if c.Lock.Locked || c.InstallStatus != "installed" || m.runtime.IsRunning(id) {
		return errors.New("stop the installed Cell before changing its resource allocation")
	}
	c.Lock = DefaultLock("resource_operation", "Resource allocation in progress", "A resource operation must finish before this Cell can start.")
	c.Lock.OperationToken = token
	if err := saveLock(c.Dir, c.Lock); err != nil {
		c.Lock = CellLock{}
		return err
	}
	return nil
}
func (m *Manager) EndOperation(id, token string) error {
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.mutex.Lock()
	defer m.mutex.Unlock()
	c, ok := m.cells[id]
	if !ok {
		return errors.New("Cell not found")
	}
	if c.Lock.Type != "resource_operation" || c.Lock.OperationToken != token {
		return errors.New("operation token does not match")
	}
	if err := deleteLock(c.Dir); err != nil {
		return err
	}
	c.Lock = CellLock{}
	return nil
}
func (c *Cell) exclusiveOperation() bool {
	return c.Lock.Type == "content_install" || c.Lock.Type == "instance_operation" || c.Lock.Type == "instance_recovery" || c.Lock.Type == "resource_operation"
}
