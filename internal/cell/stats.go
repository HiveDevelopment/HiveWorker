package cell

import (
	"errors"
	"time"
)

func (m *Manager) Stats(id string) (*CellStats, error) {
	m.mutex.RLock()

	cell, exists := m.cells[id]
	if !exists {
		m.mutex.RUnlock()
		return nil, errors.New("cell not found")
	}

	cellCopy := *cell
	m.mutex.RUnlock()

	stats := &CellStats{
		ID:                   cellCopy.ID,
		ConfigurationPending: cellCopy.ConfigurationPending,
		Running:              false,
		UptimeSec:            0,
		DiskBytes:            0,
		CPU:                  0,
		MemoryMB:             0,
	}

	stats.Running = m.runtime.IsRunning(id)

	if startedAt := m.runtime.StartedAt(id); stats.Running && startedAt != nil {
		stats.UptimeSec = int64(time.Since(*startedAt).Seconds())
	}

	runtimeStats, err := m.runtime.Stats(id)
	if err == nil && runtimeStats != nil {
		stats.CPU = runtimeStats.CPU
		stats.MemoryMB = runtimeStats.MemoryMB
		stats.PID = runtimeStats.PID
		stats.NetworkRxBytes = runtimeStats.NetworkRxBytes
		stats.NetworkTxBytes = runtimeStats.NetworkTxBytes
	}

	// Serve the last measurement; scan in the background rather than delaying polling.
	stats.DiskBytes, stats.DiskReady = m.cachedDisk(id, cellCopy.Dir)

	return stats, nil
}
