package cell

import (
	"encoding/json"
	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/comb"
	"hivepanel-worker/internal/minecraft"
	"os"
	"path/filepath"
	"testing"
)

func contentFixture(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), t.TempDir(), comb.NewManager(t.TempDir()), &definitionRuntime{}, allocation.NewManager(nil), nil)
	m.cells["cell"] = &Cell{ID: "cell", Name: "Cell", Comb: "minecraft", CombData: map[string]any{"id": "minecraft", "game": "minecraft"}, Dir: t.TempDir(), InstallStatus: "installed", Startup: StartupConfig{Command: "old command"}}
	return m
}
func TestContentReservationGuardsAllDestructiveLifecycleOperations(t *testing.T) {
	m := contentFixture(t)
	m.cells["cell"].Lock = CellLock{Locked: true, Type: "content_install"}
	for name, action := range map[string]func() error{"start": func() error { return m.Start("cell") }, "reinstall": func() error { return m.Reinstall("cell") }, "delete": func() error { return m.Delete("cell") }, "unlock": func() error { return m.UnlockCell("cell") }, "install": func() error { return m.StartInstall("cell") }} {
		if action() == nil {
			t.Errorf("%s ignored content reservation", name)
		}
	}
	_, err := m.UpdateDefinition("cell", UpdateCellDefinitionRequest{Name: "Cell", Comb: "minecraft", CombData: map[string]any{"id": "minecraft"}})
	if err == nil {
		t.Fatal("definition update ignored reservation")
	}
}
func TestCompletedContentMustBeAcknowledgedBeforeDefinitionChanges(t *testing.T) {
	m := contentFixture(t)
	m.contentJobs["cell"] = &ContentJob{ID: "job", State: "complete"}
	request := UpdateCellDefinitionRequest{Name: "Cell", Comb: "minecraft", CombData: map[string]any{"id": "minecraft"}, Allocation: allocation.Allocation{IP: "127.0.0.1", Port: 30000}}
	if _, err := m.UpdateDefinition("cell", request); err == nil {
		t.Fatal("old panel definition accepted")
	}
	m.allocManager = allocation.NewManager([]allocation.Allocation{request.Allocation})
	request.ContentJobID = "job"
	if _, err := m.UpdateDefinition("cell", request); err != nil {
		t.Fatal(err)
	}
}
func TestCommittedContentRestoresStartupAfterWorkerRestart(t *testing.T) {
	m := contentFixture(t)
	c := m.cells["cell"]
	backup := filepath.Join(c.Dir, ".hivepanel", "content-backups", "fixture")
	os.MkdirAll(backup, 0750)
	os.WriteFile(filepath.Join(backup, "journal.json"), []byte(`{"committed":true,"entries":[]}`), 0600)
	job := &ContentJob{ID: "job", State: "installing", Logs: []string{}, Plan: minecraft.Plan{Minecraft: "1.21.1", Loader: "fabric", LoaderVersion: "0.16.0"}, Result: &minecraft.Result{Startup: "new command", Backup: backup}}
	m.contentJobs[c.ID] = job
	c.Lock = CellLock{Locked: true, Type: "content_install"}
	m.recoverContent(c)
	if c.Startup.Command != "new command" || c.Variables["version"] != "1.21.1" || job.State != "complete" || c.Lock.Locked {
		data, _ := json.Marshal(c)
		t.Fatalf("recovery failed: %s %+v", data, job)
	}
}
