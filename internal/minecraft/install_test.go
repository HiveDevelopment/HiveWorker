package minecraft

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixturePlan(origin string, data []byte) Plan {
	sum := sha512.Sum512(data)
	return Plan{Format: 1, Kind: "mod", Minecraft: "1.21.1", Loader: "fabric", Files: []File{{Path: "mods/new.jar", URL: origin + "/api/v1/minecraft/artifacts/" + strings.Repeat("a", 64), Size: int64(len(data)), Hashes: map[string]string{"sha512": hex.EncodeToString(sum[:])}}}}
}
func TestValidateRejectsUnsafePlans(t *testing.T) {
	valid := fixturePlan("https://registry.example", []byte("jar"))
	if err := Validate(valid, "https://registry.example"); err != nil {
		t.Fatal(err)
	}
	cases := []string{"../escape", "mods/../../escape", "/etc/passwd", "mods\\escape", ".hivepanel/lock.json", ".hivepanel", "mods//bad"}
	for _, path := range cases {
		p := fixturePlan("https://registry.example", []byte("jar"))
		p.Files[0].Path = path
		if Validate(p, "https://registry.example") == nil {
			t.Errorf("accepted %s", path)
		}
	}
	for _, u := range []string{"https://evil.example/api/v1/minecraft/artifacts/" + strings.Repeat("a", 64), "https://registry.example/not-artifact", "http://registry.example/api/v1/minecraft/artifacts/" + strings.Repeat("a", 64)} {
		p := fixturePlan("https://registry.example", []byte("jar"))
		p.Files[0].URL = u
		if Validate(p, "https://registry.example") == nil {
			t.Errorf("accepted %s", u)
		}
	}
	valid.Files = append(valid.Files, valid.Files[0])
	if Validate(valid, "https://registry.example") == nil {
		t.Fatal("duplicate accepted")
	}
}
func runFixture(t *testing.T, mutate func(*Plan), checkpoint func(Result) error) (string, Result, error) {
	t.Helper()
	data := []byte("verified new jar")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer server.Close()
	old := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = old }()
	root := filepath.Join(t.TempDir(), "cell")
	if err := os.MkdirAll(filepath.Join(root, "mods"), 0750); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "mods", "old.jar"), []byte("old"), 0640)
	os.MkdirAll(filepath.Join(root, "world"), 0750)
	os.WriteFile(filepath.Join(root, "world", "region.dat"), []byte("world"), 0640)
	os.MkdirAll(filepath.Join(root, "plugins"), 0750)
	os.WriteFile(filepath.Join(root, "plugins", "legacy.jar"), []byte("plugin"), 0640)
	os.MkdirAll(filepath.Join(root, ".hivepanel"), 0750)
	os.WriteFile(filepath.Join(root, ".hivepanel", "keep.json"), []byte("management"), 0640)
	plan := fixturePlan(server.URL, data)
	if mutate != nil {
		mutate(&plan)
	}
	result, err := Install(context.Background(), root, "", server.URL, plan, func(string) {}, func(string) error { return nil }, 0, 0, checkpoint)
	return root, result, err
}
func TestInstallVerifiesThenReplacesModsAndRetainsBackup(t *testing.T) {
	root, result, err := runFixture(t, func(p *Plan) { p.Kind = "builder"; p.ReplaceMods = true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "mods", "new.jar"))
	if string(data) != "verified new jar" {
		t.Fatal("new jar missing")
	}
	if _, err := os.Stat(filepath.Join(root, "mods", "old.jar")); !os.IsNotExist(err) {
		t.Fatal("old mod remained")
	}
	data, _ = os.ReadFile(filepath.Join(result.Backup, "original", "mods", "old.jar"))
	if string(data) != "old" {
		t.Fatal("backup missing")
	}
	if !TransactionCommitted(result.Backup) {
		t.Fatal("transaction not committed")
	}
}
func TestChecksumFailureLeavesExistingFilesUntouched(t *testing.T) {
	root, _, err := runFixture(t, func(p *Plan) { p.Files[0].Hashes["sha512"] = strings.Repeat("0", 128) }, nil)
	if err == nil {
		t.Fatal("bad hash accepted")
	}
	data, _ := os.ReadFile(filepath.Join(root, "mods", "old.jar"))
	if string(data) != "old" {
		t.Fatal("old file changed")
	}
	if _, err := os.Stat(filepath.Join(root, "mods", "new.jar")); !os.IsNotExist(err) {
		t.Fatal("unverified file installed")
	}
}
func TestCommitFailureRollsBackReplacedMods(t *testing.T) {
	root, _, err := runFixture(t, func(p *Plan) { p.Kind = "builder"; p.ReplaceMods = true }, func(Result) error { return errors.New("checkpoint disk failure") })
	if err == nil {
		t.Fatal("checkpoint failure ignored")
	}
	data, _ := os.ReadFile(filepath.Join(root, "mods", "old.jar"))
	if string(data) != "old" {
		t.Fatal("old mods not restored")
	}
	if _, err := os.Stat(filepath.Join(root, "mods", "new.jar")); !os.IsNotExist(err) {
		t.Fatal("failed mod persisted")
	}
}
func TestRecoveryRestoresInterruptedTransaction(t *testing.T) {
	root := t.TempDir()
	backup := filepath.Join(root, ".hivepanel", "content-backups", "fixture")
	os.MkdirAll(filepath.Join(backup, "original", "mods"), 0750)
	os.MkdirAll(filepath.Join(root, "mods"), 0750)
	os.WriteFile(filepath.Join(backup, "original", "mods", "old.jar"), []byte("old"), 0640)
	os.WriteFile(filepath.Join(root, "mods", "old.jar"), []byte("new"), 0640)
	state := journal{Entries: []journalEntry{{Path: "mods/old.jar", Original: true}}}
	if err := saveJournal(backup, state); err != nil {
		t.Fatal(err)
	}
	if TransactionCommitted(backup) {
		t.Fatal("premature commit")
	}
	if err := Recover(root); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "mods", "old.jar"))
	if string(data) != "old" {
		t.Fatal("not restored")
	}
}
func TestSymlinkTargetNeverWritesOutsideCell(t *testing.T) {
	outside := t.TempDir()
	// Direct path validation covers the link before the transaction's rename.
	cell := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cell, "mods")); err != nil {
		t.Skip(err)
	}
	if noSymlinks(cell, filepath.Join("mods", "new.jar")) == nil {
		t.Fatal("symlink accepted")
	}
}
func TestWipeReplacesWholeServerAndPreservesManagementState(t *testing.T) {
	root, result, err := runFixture(t, func(p *Plan) { p.Kind = "modpack"; p.Wipe = true; p.ReplaceMods = true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "mods", "old.jar")); !os.IsNotExist(err) {
		t.Fatal("old server content remained")
	}
	for _, path := range []string{"world", "plugins"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("%s was not wiped", path)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, ".hivepanel", "keep.json")); string(data) != "management" {
		t.Fatal("management state was wiped")
	}
	if data, _ := os.ReadFile(filepath.Join(result.Backup, "original", "world", "region.dat")); string(data) != "world" {
		t.Fatal("world backup missing")
	}

	if _, err := os.Stat(filepath.Join(root, "mods", "new.jar")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(result.Backup, "original", "mods", "old.jar")); err != nil {
		t.Fatal("old files were not retained", err)
	}
}
func TestWipeCheckpointFailureRestoresWholeDirectories(t *testing.T) {
	root, _, err := runFixture(t, func(p *Plan) { p.Kind = "modpack"; p.Wipe = true }, func(Result) error { return errors.New("disk error") })
	if err == nil {
		t.Fatal("failure ignored")
	}
	data, _ := os.ReadFile(filepath.Join(root, "mods", "old.jar"))
	if string(data) != "old" {
		t.Fatal("original nested files lost")
	}
	if _, err := os.Stat(filepath.Join(root, "mods", "new.jar")); !os.IsNotExist(err) {
		t.Fatal("new mod persisted")
	}
}
func TestConfigureServerUsesAllocationAndPriorEULAAcceptance(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "server.properties"), []byte("motd=Pack\nserver-port=25565\n"), 0640)
	if err := ConfigureServer(root, 25570, true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "server.properties"))
	if string(data) != "motd=Pack\nserver-port=25570\n" {
		t.Fatal(string(data))
	}
	if !EULAAccepted(root) {
		t.Fatal("EULA acceptance not preserved")
	}
}
func TestRolledBackJournalIsNotSuccessfulCommit(t *testing.T) {
	backup := t.TempDir()
	if err := saveJournal(backup, journal{Committed: true, RolledBack: true}); err != nil {
		t.Fatal(err)
	}
	if TransactionCommitted(backup) {
		t.Fatal("rollback was treated as successful install")
	}
}
