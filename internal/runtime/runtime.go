package runtime

import (
	"hivepanel-worker/internal/allocation"
	"time"
)

type RuntimeCell struct {
	ID          string
	Command     string
	InstanceDir string

	Image      string
	WorkingDir string

	Environment map[string]string

	AllocationIP   string
	AllocationPort int
	Allocations    []allocation.Allocation

	Limits Limits
}

type Stats struct {
	CPU            float64 `json:"cpu"`
	MemoryMB       float64 `json:"memory_mb"`
	PID            int     `json:"pid,omitempty"`
	NetworkRxBytes uint64  `json:"network_rx_bytes"`
	NetworkTxBytes uint64  `json:"network_tx_bytes"`
}

type Runtime interface {
	Start(cell RuntimeCell, onOutput func(line string), onExit func()) error
	Stop(id string) error
	SendCommand(id string, command string) error
	Stats(id string) (*Stats, error)
	IsRunning(id string) bool
	StartedAt(id string) *time.Time
}

type StopOptions struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
	Signal  string `json:"signal"`
}

type ConfigurableStopRuntime interface {
	StopConfigured(id string, options StopOptions) error
}

type RecoverableRuntime interface {
	Recover(cellIDs []string, onOutput func(cellID string, line string), onExit func(cellID string)) error
}

type DeletableRuntime interface {
	Delete(id string) error
}

type Limits struct {
	MemoryMB         int    `json:"memory_mb"`
	OverheadMemoryMB int    `json:"overhead_memory_mb"`
	SwapMB           int    `json:"swap_mb"`
	CPUPercent       int    `json:"cpu_percent"`
	CPUPinning       string `json:"cpu_pinning,omitempty"`
	DiskMB           int    `json:"disk_mb"`
	IOWeight         int    `json:"io_weight"`
	OOMKiller        bool   `json:"oom_killer"`
}
