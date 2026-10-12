package cell

import hiveruntime "hivepanel-worker/internal/runtime"

func (m *Manager) RecoverRuntime() error {
	recoverable, ok := m.runtime.(hiveruntime.RecoverableRuntime)
	if !ok {
		return nil
	}

	m.mutex.Lock()

	ids := make([]string, 0, len(m.cells))
	for id := range m.cells {
		ids = append(ids, id)
	}

	m.mutex.Unlock()

	err := recoverable.Recover(ids, func(cellID string, line string) {
		m.mutex.Lock()
		if cell, exists := m.cells[cellID]; exists {
			m.broadcast(cell, line)
		}
		m.mutex.Unlock()
	}, func(cellID string) {
		_ = m.releaseStoppedAllocations(cellID)
		m.mutex.Lock()
		if cell, exists := m.cells[cellID]; exists {
			m.broadcast(cell, "Cell stopped.")
		}
		m.mutex.Unlock()
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := m.releaseStoppedAllocations(id); err != nil {
			return err
		}
	}
	return nil
}
