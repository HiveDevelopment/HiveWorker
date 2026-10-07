package cell

import (
	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/backup"
	"hivepanel-worker/internal/comb"
	hiveruntime "hivepanel-worker/internal/runtime"
	"sync"
)

type FeatureLimits struct {
	DatabaseLimit   *int `json:"database_limit,omitempty"`
	AllocationLimit *int `json:"allocation_limit,omitempty"`
	BackupLimit     *int `json:"backup_limit,omitempty"`
	BackupStorageMB *int `json:"backup_storage_mb,omitempty"`
}

type DockerConfig struct {
	Image string `json:"image,omitempty"`
}

type StartupConfig struct {
	Command string `json:"command,omitempty"`
}

type Cell struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	CreatedAt string `json:"created_at"`
	Status    string `json:"status"`

	Comb                  string                  `json:"comb"`
	CombData              map[string]any          `json:"comb_data,omitempty"`
	Variables             map[string]string       `json:"variables"`
	Allocation            allocation.Allocation   `json:"allocation"`
	AdditionalAllocations []allocation.Allocation `json:"additional_allocations"`
	Limits                hiveruntime.Limits      `json:"limits"`
	FeatureLimits         FeatureLimits           `json:"feature_limits"`
	Docker                DockerConfig            `json:"docker"`
	Startup               StartupConfig           `json:"startup"`

	console     []ConsoleEntry
	subscribers map[chan ConsoleEntry]bool

	Lock CellLock `json:"lock"`
}

type CreateCellRequest struct {
	ID                    string                  `json:"id,omitempty"`
	Name                  string                  `json:"name"`
	Comb                  string                  `json:"comb"`
	CombData              map[string]any          `json:"comb_data,omitempty"`
	Variables             map[string]string       `json:"variables"`
	Limits                hiveruntime.Limits      `json:"limits"`
	FeatureLimits         FeatureLimits           `json:"feature_limits"`
	Docker                DockerConfig            `json:"docker"`
	Startup               StartupConfig           `json:"startup"`
	Allocation            allocation.Allocation   `json:"allocation"`
	AdditionalAllocations []allocation.Allocation `json:"additional_allocations,omitempty"`
}

type UpdateCellDefinitionRequest struct {
	Name                  string                  `json:"name"`
	Comb                  string                  `json:"comb"`
	CombData              map[string]any          `json:"comb_data"`
	Variables             map[string]string       `json:"variables"`
	Allocation            allocation.Allocation   `json:"allocation"`
	AdditionalAllocations []allocation.Allocation `json:"additional_allocations"`
	Limits                hiveruntime.Limits      `json:"limits"`
	FeatureLimits         FeatureLimits           `json:"feature_limits"`
	Docker                DockerConfig            `json:"docker"`
	Startup               StartupConfig           `json:"startup"`
}

type CellStats struct {
	ID             string  `json:"id"`
	Running        bool    `json:"running"`
	UptimeSec      int64   `json:"uptime_sec"`
	DiskBytes      int64   `json:"disk_bytes"`
	CPU            float64 `json:"cpu"`
	MemoryMB       float64 `json:"memory_mb"`
	PID            int     `json:"pid,omitempty"`
	NetworkRxBytes uint64  `json:"network_rx_bytes"`
	NetworkTxBytes uint64  `json:"network_tx_bytes"`
}

type CellLock struct {
	Locked bool `json:"locked"`

	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`

	DisablePower    bool `json:"disable_power"`
	DisableConsole  bool `json:"disable_console"`
	DisableFiles    bool `json:"disable_files"`
	DisableBackups  bool `json:"disable_backups"`
	DisableSettings bool `json:"disable_settings"`
	DisableImporter bool `json:"disable_importer"`
}

type Manager struct {
	mutex           sync.RWMutex
	cells           map[string]*Cell
	dataDir         string
	instancesDir    string
	combManager     *comb.Manager
	runtime         hiveruntime.Runtime
	allocManager    *allocation.Manager
	backupManager   *backup.Manager
	consoleSessions map[string]ConsoleSession
}
