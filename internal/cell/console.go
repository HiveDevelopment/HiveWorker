package cell

import (
	"errors"
	"time"
)

type ConsoleEntry struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

func (m *Manager) Console(id string) ([]ConsoleEntry, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	cell, exists := m.cells[id]
	if !exists {
		return nil, errors.New("cell not found")
	}

	lines := make([]ConsoleEntry, len(cell.console))
	copy(lines, cell.console)

	return lines, nil
}

func (m *Manager) ClearConsole(id string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	cell, exists := m.cells[id]
	if !exists {
		return errors.New("cell not found")
	}

	cell.console = nil

	return nil
}

func (m *Manager) Subscribe(id string) (chan ConsoleEntry, error) {
	return m.SubscribeWithHistory(id, true)
}

func (m *Manager) SubscribeWithHistory(id string, includeHistory bool) (chan ConsoleEntry, error) {
	ch := make(chan ConsoleEntry, 100)

	m.mutex.Lock()
	defer m.mutex.Unlock()

	cell, exists := m.cells[id]
	if !exists {
		close(ch)
		return nil, errors.New("cell not found")
	}

	if cell.subscribers == nil {
		cell.subscribers = map[chan ConsoleEntry]bool{}
	}

	cell.subscribers[ch] = true

	if includeHistory {
		for _, line := range cell.console {
			select {
			case ch <- line:
			default:
				return ch, nil
			}
		}
	}

	return ch, nil
}

func (m *Manager) Unsubscribe(id string, ch chan ConsoleEntry) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	cell, exists := m.cells[id]
	if !exists {
		return
	}

	if cell.subscribers != nil {
		delete(cell.subscribers, ch)
	}

	close(ch)
}

func (m *Manager) broadcast(cell *Cell, line string) {
	entry := ConsoleEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Message:   line,
	}

	cell.console = append(cell.console, entry)
	m.trimConsole(cell)

	if cell.subscribers == nil {
		return
	}

	for ch := range cell.subscribers {
		select {
		case ch <- entry:
		default:
			// Do not block the whole manager if a client is slow/disconnected.
		}
	}
}

func (m *Manager) trimConsole(cell *Cell) {
	maxLines := 500

	if len(cell.console) > maxLines {
		cell.console = cell.console[len(cell.console)-maxLines:]
	}
}
