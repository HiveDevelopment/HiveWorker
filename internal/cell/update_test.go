package cell

import (
	"errors"
	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/comb"
	hiveruntime "hivepanel-worker/internal/runtime"
	"os"
	"testing"
	"time"
)

type definitionRuntime struct {
	running bool
	started hiveruntime.RuntimeCell
}

func (r *definitionRuntime) Start(c hiveruntime.RuntimeCell, output func(string), exit func()) error {
	r.started = c
	r.running = true
	return nil
}
func (r *definitionRuntime) Stop(string) error                { r.running = false; return nil }
func (r *definitionRuntime) SendCommand(string, string) error { return nil }
func (r *definitionRuntime) Stats(string) (*hiveruntime.Stats, error) {
	return &hiveruntime.Stats{}, nil
}
func (r *definitionRuntime) IsRunning(string) bool       { return r.running }
func (r *definitionRuntime) StartedAt(string) *time.Time { return nil }

func TestRunningDefinitionIsQueuedAndOldPortRemainsReserved(t *testing.T) {
	old := allocation.Allocation{IP: "127.0.0.1", Port: 31001}
	added := allocation.Allocation{IP: "127.0.0.1", Port: 31002}
	allocator := allocation.NewManager([]allocation.Allocation{old, added})
	allocator.ReserveExisting(old)
	runtime := &definitionRuntime{running: true}
	manager := NewManager(t.TempDir(), t.TempDir(), comb.NewManager(t.TempDir()), runtime, allocator, nil)
	manifest := map[string]any{"id": "steam-test", "name": "Steam", "startup": "test", "image": "test"}
	manager.cells["test"] = &Cell{ID: "test", Name: "old", Comb: "steam-test", CombData: manifest, Allocation: old}
	request := UpdateCellDefinitionRequest{Name: "new", Comb: "steam-test", CombData: manifest, Allocation: added}
	updated, err := manager.UpdateDefinition("test", request)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ConfigurationPending || updated.Allocation != added || !runtime.running {
		t.Fatal("update interrupted runtime or failed to queue")
	}
	if !allocator.IsReserved(old) || !allocator.IsReserved(added) {
		t.Fatal("old live and new desired ports must both be reserved")
	}
	// A Worker restart must retain the queued definition and both reservations.
	reloaded := NewManager(manager.dataDir, manager.instancesDir, comb.NewManager(t.TempDir()), runtime, allocation.NewManager([]allocation.Allocation{old, added}), nil)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if !reloaded.cells["test"].ConfigurationPending || !reloaded.allocManager.IsReserved(old) {
		t.Fatal("queue did not survive reload")
	}
	runtime.running = false
	updated, err = manager.UpdateDefinition("test", request)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ConfigurationPending || allocator.IsReserved(old) || !allocator.IsReserved(added) {
		t.Fatal("stopped update failed to release old reservation")
	}
}

func TestFailedSaveRestoresDefinitionAndReservations(t *testing.T) {
	old := allocation.Allocation{IP: "127.0.0.1", Port: 31011}
	allocator := allocation.NewManager([]allocation.Allocation{old})
	allocator.ReserveExisting(old)
	blocked := t.TempDir() + "/file"
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(blocked, t.TempDir(), comb.NewManager(t.TempDir()), &definitionRuntime{running: true}, allocator, nil)
	manifest := map[string]any{"id": "test"}
	manager.cells["test"] = &Cell{ID: "test", Name: "original", Comb: "test", CombData: manifest, Allocation: old}
	_, err := manager.UpdateDefinition("test", UpdateCellDefinitionRequest{Name: "changed", Comb: "test", CombData: manifest, Allocation: old})
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected save failure, got %v", err)
	}
	if manager.cells["test"].Name != "original" || manager.cells["test"].ConfigurationPending || !allocator.IsReserved(old) {
		t.Fatal("failed update mutated live definition")
	}
}

type configuredRuntime struct {
	definitionRuntime
	stopped chan hiveruntime.StopOptions
}

func (r *configuredRuntime) StopConfigured(id string, options hiveruntime.StopOptions) error {
	r.stopped <- options
	return nil
}

func TestCurrentCombStopOverrideCanStopExistingCell(t *testing.T) {
	runtime := &configuredRuntime{stopped: make(chan hiveruntime.StopOptions, 1)}
	manager := NewManager(t.TempDir(), t.TempDir(), comb.NewManager(t.TempDir()), runtime, allocation.NewManager(nil), nil)
	manager.cells["test"] = &Cell{ID: "test", ActiveStop: &hiveruntime.StopOptions{Command: "stop", Timeout: 30}}
	requested := hiveruntime.StopOptions{Command: "quit", Timeout: 90, Signal: "SIGINT"}
	if err := manager.StopWithOptions("test", &requested); err != nil {
		t.Fatal(err)
	}
	select {
	case actual := <-runtime.stopped:
		if actual != requested {
			t.Fatalf("used old shutdown policy: %v", actual)
		}
	case <-time.After(time.Second):
		t.Fatal("stop request was not delivered")
	}
}

func TestExitedRuntimeReleasesRemovedPortsWithoutLosingPendingChanges(t *testing.T) {
	old := allocation.Allocation{IP: "127.0.0.1", Port: 35001}
	desired := allocation.Allocation{IP: "127.0.0.1", Port: 35002}
	allocator := allocation.NewManager([]allocation.Allocation{old, desired})
	allocator.ReserveMany([]allocation.Allocation{old, desired})
	runtime := &definitionRuntime{}
	manager := NewManager(t.TempDir(), t.TempDir(), comb.NewManager(t.TempDir()), runtime, allocator, nil)
	manager.cells["test"] = &Cell{ID: "test", Allocation: desired, ActiveAllocations: []allocation.Allocation{old}, ConfigurationPending: true}
	if err := manager.releaseStoppedAllocations("test"); err != nil {
		t.Fatal(err)
	}
	if allocator.IsReserved(old) || !allocator.IsReserved(desired) || !manager.cells["test"].ConfigurationPending {
		t.Fatal("exit lost pending definition or retained removed port")
	}
}
