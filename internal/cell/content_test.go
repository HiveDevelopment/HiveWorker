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
func TestPublicContentStatusKeepsPathsRelativeWithoutChangingRecoveryState(t *testing.T) {
	m := contentFixture(t)
	root := m.cells["cell"].Dir
	backup := filepath.Join(root, ".hivepanel", "content-backups", "job")
	m.contentJobs["cell"] = &ContentJob{ID: "job", State: "complete", Message: "Previous files in " + backup, Logs: []string{"Failed file " + filepath.Join(root, "config.json")}, Result: &minecraft.Result{Backup: backup}}
	public, err := m.ContentStatus("cell")
	if err != nil {
		t.Fatal(err)
	}
	if public.Result.Backup != ".hivepanel/content-backups/job" || public.Logs[0] != "Failed file config.json" {
		t.Fatalf("host paths exposed: %+v", public)
	}
	if m.contentJobs["cell"].Result.Backup != backup || m.contentJobs["cell"].Logs[0] == public.Logs[0] {
		t.Fatal("public projection changed durable recovery state")
	}
}

func TestCommittedPreinstallRestoresRuntimeWithoutMinecraftVariables(t *testing.T) {
	m := contentFixture(t)
	c := m.cells["cell"]
	c.Variables = map[string]string{"memory": "2048", "DISCORD_TOKEN": "private-token"}
	backup := filepath.Join(c.Dir, ".hivepanel", "content-backups", "preset")
	os.MkdirAll(backup, 0750)
	os.WriteFile(filepath.Join(backup, "journal.json"), []byte(`{"committed":true,"entries":[]}`), 0600)
	job := &ContentJob{ID: "preset", State: "installing", Plan: minecraft.Plan{Kind: "preinstall", Runtime: &minecraft.PreinstallRuntime{Image: "node:22", Startup: "node app.js", Environment: map[string]string{"DISCORD_TOKEN": "{{DISCORD_TOKEN}}"}, VariablesSchema: []map[string]any{{"name": "DISCORD_TOKEN", "default": ""}, {"name": "SUPER", "default": ""}}}}, Result: &minecraft.Result{Startup: "node app.js", Image: "node@sha256:verified", Backup: backup}}
	m.contentJobs[c.ID] = job
	c.Lock = CellLock{Locked: true, Type: "content_install"}
	m.recoverContent(c)
	if c.Startup.Command != "node app.js" || c.Docker.Image != "node@sha256:verified" || c.CombData["image"] != c.Docker.Image || job.State != "complete" || c.Lock.Locked {
		t.Fatal("preinstall runtime recovery failed")
	}
	if c.Variables["DISCORD_TOKEN"] != "private-token" || c.Variables["memory"] != "2048" {
		t.Fatal("existing variables lost")
	}
	if _, exists := c.Variables["loader_version"]; exists {
		t.Fatal("Minecraft variables added to bot")
	}
}

type deletionRuntime struct {
	*definitionRuntime
	fail   bool
	called bool
}

func (r *deletionRuntime) Delete(id string) error {
	r.called = true
	if r.fail {
		return os.ErrPermission
	}
	return nil
}
func TestCellDeletionPreservesStateWhenContainerRemovalFails(t *testing.T) {
	m := contentFixture(t)
	c := m.cells["cell"]
	runtime := &deletionRuntime{definitionRuntime: &definitionRuntime{}, fail: true}
	m.runtime = runtime
	if err := m.Delete("cell"); err == nil {
		t.Fatal("Docker deletion failure ignored")
	}
	if !runtime.called || m.cells["cell"] == nil {
		t.Fatal("Cell removed despite failed Docker deletion")
	}
	if _, err := os.Stat(c.Dir); err != nil {
		t.Fatal("files deleted despite failed Docker deletion")
	}
	runtime.fail = false
	if err := m.Delete("cell"); err != nil {
		t.Fatal(err)
	}
	if m.cells["cell"] != nil {
		t.Fatal("Cell record retained after deletion")
	}
	if _, err := os.Stat(c.Dir); !os.IsNotExist(err) {
		t.Fatal("Cell files retained after deletion")
	}
}
