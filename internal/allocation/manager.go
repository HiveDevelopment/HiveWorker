package allocation

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
)

type Allocation struct {
	IP   string `json:"ip" yaml:"ip"`
	Port int    `json:"port" yaml:"port"`
}

type Manager struct {
	mutex   sync.Mutex
	allowed []Allocation
	used    map[string]bool
}

func NewManager(allocations []Allocation) *Manager {
	return &Manager{
		allowed: normaliseAllocations(allocations),
		used:    map[string]bool{},
	}
}

func (m *Manager) Configuration() []Allocation {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	return cloneAllocations(m.allowed)
}

func (m *Manager) Reconfigure(allocations []Allocation) ([]Allocation, error) {
	allocations = normaliseAllocations(allocations)

	if err := validateConfiguration(allocations); err != nil {
		return nil, err
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	allowed := allocationSet(allocations)

	for key := range m.used {
		if !allowed[key] {
			return nil, fmt.Errorf(
				"cannot remove allocation %s while an existing Cell still uses it",
				key,
			)
		}
	}

	m.allowed = cloneAllocations(allocations)

	return cloneAllocations(m.allowed), nil
}

func (m *Manager) IPs() []string {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	result := make([]string, 0)
	seen := map[string]bool{}

	for _, item := range m.allowed {
		if seen[item.IP] {
			continue
		}

		seen[item.IP] = true
		result = append(result, item.IP)
	}

	return result
}

func (m *Manager) ReserveExisting(allocation Allocation) {
	allocation = normaliseAllocation(allocation)

	if !validAllocation(allocation) {
		return
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.used[allocationKey(allocation)] = true
}

func (m *Manager) ReserveMany(allocations []Allocation) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for _, item := range allocations {
		item = normaliseAllocation(item)

		if !validAllocation(item) {
			continue
		}

		m.used[allocationKey(item)] = true
	}
}

func (m *Manager) ReplaceReservations(oldAllocations []Allocation, newAllocations []Allocation) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for _, item := range oldAllocations {
		item = normaliseAllocation(item)

		if !validAllocation(item) {
			continue
		}

		delete(m.used, allocationKey(item))
	}

	for _, item := range newAllocations {
		item = normaliseAllocation(item)

		if !validAllocation(item) {
			continue
		}

		m.used[allocationKey(item)] = true
	}
}

func (m *Manager) Allocate() (Allocation, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if len(m.allowed) == 0 {
		return Allocation{}, errors.New("no allocations configured")
	}

	for _, item := range m.allowed {
		key := allocationKey(item)

		if m.used[key] {
			continue
		}

		if !isPortFree(item.IP, item.Port) {
			continue
		}

		m.used[key] = true

		return item, nil
	}

	return Allocation{}, errors.New("no free allocations available")
}

func (m *Manager) AllocateForIP(ip string) (Allocation, error) {
	ip = strings.TrimSpace(ip)

	if ip == "" {
		return Allocation{}, errors.New("allocation IP is required")
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	configured := false

	for _, item := range m.allowed {
		if item.IP != ip {
			continue
		}

		configured = true

		key := allocationKey(item)

		if m.used[key] {
			continue
		}

		if !isPortFree(item.IP, item.Port) {
			continue
		}

		m.used[key] = true

		return item, nil
	}

	if !configured {
		return Allocation{}, errors.New("allocation IP is not configured")
	}

	return Allocation{}, errors.New("no free allocations available for IP")
}

func (m *Manager) IsReserved(allocation Allocation) bool {
	allocation = normaliseAllocation(allocation)

	if !validAllocation(allocation) {
		return false
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	return m.used[allocationKey(allocation)]
}

func (m *Manager) Release(allocation Allocation) {
	allocation = normaliseAllocation(allocation)

	if !validAllocation(allocation) {
		return
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	delete(m.used, allocationKey(allocation))
}

func allocationKey(allocation Allocation) string {
	return net.JoinHostPort(
		strings.TrimSpace(allocation.IP),
		strconv.Itoa(allocation.Port),
	)
}

func isPortFree(ip string, port int) bool {
	address := net.JoinHostPort(
		strings.TrimSpace(ip),
		strconv.Itoa(port),
	)

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return false
	}

	_ = listener.Close()

	return true
}

func validateConfiguration(allocations []Allocation) error {
	for _, item := range allocations {
		if err := validateAllocation(item); err != nil {
			return err
		}
	}

	return nil
}

func validateAllocation(allocation Allocation) error {
	allocation = normaliseAllocation(allocation)

	if allocation.IP == "" {
		return errors.New("allocation IP is required")
	}

	if allocation.IP != "0.0.0.0" && allocation.IP != "::" && net.ParseIP(allocation.IP) == nil {
		return fmt.Errorf("invalid allocation IP address: %s", allocation.IP)
	}

	if allocation.Port < 1 || allocation.Port > 65535 {
		return fmt.Errorf(
			"allocation port must be between 1 and 65535 for %s",
			allocation.IP,
		)
	}

	return nil
}

func normaliseAllocations(allocations []Allocation) []Allocation {
	result := make([]Allocation, 0, len(allocations))
	seen := map[string]bool{}

	for _, item := range allocations {
		item = normaliseAllocation(item)

		if !validAllocation(item) {
			continue
		}

		key := allocationKey(item)

		if seen[key] {
			continue
		}

		seen[key] = true
		result = append(result, item)
	}

	return result
}

func normaliseAllocation(allocation Allocation) Allocation {
	allocation.IP = strings.TrimSpace(allocation.IP)

	return allocation
}

func validAllocation(allocation Allocation) bool {
	return allocation.IP != "" &&
		allocation.Port >= 1 &&
		allocation.Port <= 65535
}

func allocationSet(allocations []Allocation) map[string]bool {
	result := make(map[string]bool, len(allocations))

	for _, item := range allocations {
		result[allocationKey(item)] = true
	}

	return result
}

func cloneAllocations(allocations []Allocation) []Allocation {
	return append([]Allocation(nil), allocations...)
}
