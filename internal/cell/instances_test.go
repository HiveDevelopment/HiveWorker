package cell

import (
	"github.com/google/uuid"
	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/comb"
	hiveruntime "hivepanel-worker/internal/runtime"
	"os"
	"path/filepath"
	"testing"
)

func instanceManager(t *testing.T) (*Manager, *Cell) {
	t.Helper()
	root := t.TempDir()
	m := NewManager(filepath.Join(root, "data"), filepath.Join(root, "files"), comb.NewManager(t.TempDir()), &definitionRuntime{}, allocation.NewManager(nil), nil)
	m.prepareInstance = func(string) error { return nil }
	dir := filepath.Join(root, "files", "test")
	os.MkdirAll(dir, 0700)
	c := &Cell{ID: "test", Dir: dir, Name: "Test", Comb: "minecraft", CombData: map[string]any{"id": "minecraft"}, Variables: map[string]string{"version": "one"}, InstallStatus: "installed", Limits: hiveruntime.Limits{DiskMB: 10, MemoryMB: 512}, Allocation: allocation.Allocation{IP: "127.0.0.1", Port: 25565}}
	m.cells[c.ID] = c
	if err := m.save(c); err != nil {
		t.Fatal(err)
	}
	return m, c
}
func TestInstanceActivationKeepsFilesSettingsAndAckLockTogether(t *testing.T) {
	m, c := instanceManager(t)
	os.WriteFile(filepath.Join(c.Dir, "world"), []byte("first"), 0600)
	saved, err := m.CreateInstance(c.ID, "Original", false)
	if err != nil {
		t.Fatal(err)
	}
	c.Variables["version"] = "two"
	os.WriteFile(filepath.Join(c.Dir, "world"), []byte("second"), 0600)
	token := uuid.NewString()
	activated, err := m.ActivateInstance(c.ID, saved.ID, token)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(c.Dir, "world"))
	if string(data) != "first" || activated.Variables["version"] != "one" {
		t.Fatal("instance files and definition diverged")
	}
	if !activated.Lock.Locked || activated.Lock.OperationToken != token {
		t.Fatal("missing panel synchronization lock")
	}
	if _, err = m.ActivateInstance(c.ID, saved.ID, token); err != nil {
		t.Fatal("lost response was not retryable", err)
	}
	if err = m.EndOperation(c.ID, token); err != nil {
		t.Fatal(err)
	}
	if _, err = m.ActivateInstance(c.ID, "default"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(c.Dir, "world"))
	if string(data) != "second" || c.Variables["version"] != "two" {
		t.Fatal("original instance lost")
	}
}
func TestInstanceQuotaAndRunningGuard(t *testing.T) {
	m, c := instanceManager(t)
	m.runtime.(*definitionRuntime).running = true
	if _, err := m.CreateInstance(c.ID, "bad", false); err == nil {
		t.Fatal("copied a running Cell")
	}
	m.runtime.(*definitionRuntime).running = false
	c.Limits.DiskMB = 1
	f, err := os.Create(filepath.Join(c.Dir, "large"))
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(700000)
	f.Close()
	if _, err = m.CreateInstance(c.ID, "too large", false); err == nil {
		t.Fatal("shared disk budget exceeded")
	}
	if c.Lock.Locked {
		t.Fatal("failed copy left Cell locked")
	}
}

func TestSwappedInstanceRecoversItsDefinitionAndPanelAck(t *testing.T) {
	m, c := instanceManager(t)
	os.WriteFile(filepath.Join(c.Dir, "world"), []byte("saved"), 0600)
	saved, err := m.CreateInstance(c.ID, "Saved", false)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.loadInstanceState(c)
	if err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	journal := instanceJournal{From: state.Active, To: saved.ID, State: state, Phase: "swapped", Token: token}
	os.Rename(c.Dir, filepath.Join(m.instanceRoot(c.ID), state.Active))
	os.Rename(filepath.Join(m.instanceRoot(c.ID), saved.ID), c.Dir)
	if err = writeInstanceJSON(m.instanceJournalPath(c.ID), journal); err != nil {
		t.Fatal(err)
	}
	c.Comb = "wrong"
	c.Variables = map[string]string{"version": "wrong"}
	if err = m.recoverInstance(c); err != nil {
		t.Fatal(err)
	}
	if c.Comb != "minecraft" || c.Variables["version"] != "one" || c.Lock.OperationToken != token {
		t.Fatal("recovery did not preserve matching runtime and acknowledgement")
	}
	if _, err = os.Stat(m.instanceJournalPath(c.ID)); !os.IsNotExist(err) {
		t.Fatal("journal was not finalized")
	}
}
