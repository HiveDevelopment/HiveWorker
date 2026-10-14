package minecraft

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preinstallFixture() Plan {
	plan := fixturePlan("https://registry.example", []byte("readme"))
	plan.Kind = "preinstall"
	plan.Wipe = true
	plan.Runtime = &PreinstallRuntime{Image: "ghcr.io/discord-tickets/bot:4.0", Startup: "/app/scripts/start.sh", SecretKeys: []string{"ENCRYPTION_KEY"}}
	plan.Files[0].URL = "https://registry.example/api/v1/preinstalls/artifacts/" + strings.Repeat("a", 64)
	plan.Files[0].Path = "PREINSTALL.md"
	return plan
}

func TestPreinstallsRequireReplacementAndSafeRuntime(t *testing.T) {
	plan := preinstallFixture()
	if err := Validate(plan, "https://registry.example"); err != nil {
		t.Fatal(err)
	}
	plan.Wipe = false
	if Validate(plan, "https://registry.example") == nil {
		t.Fatal("non-replacement preinstall accepted")
	}
	plan.Wipe = true
	plan.Runtime.Image = "--privileged"
	if Validate(plan, "https://registry.example") == nil {
		t.Fatal("Docker argument injection accepted")
	}
	plan.Runtime.Image = "node:22"
	plan.Runtime.SecretKeys = []string{"TOKEN\nOTHER=yes"}
	if Validate(plan, "https://registry.example") == nil {
		t.Fatal("secret injection accepted")
	}
}

func TestGeneratedSecretsArePrivateAndDistinct(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, stage := range []string{first, second} {
		if err := writePreinstallSecrets(stage, []string{"ENCRYPTION_KEY"}); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(filepath.Join(stage, ".env"))
		if info.Mode().Perm() != 0600 {
			t.Fatal("secret file is not private")
		}
	}
	a, _ := os.ReadFile(filepath.Join(first, ".env"))
	b, _ := os.ReadFile(filepath.Join(second, ".env"))
	if string(a) == string(b) || len(a) != len("ENCRYPTION_KEY=")+64+1 {
		t.Fatal("invalid generated keys")
	}
	if writePreinstallSecrets(first, []string{"ENCRYPTION_KEY"}) == nil {
		t.Fatal("existing configuration overwritten")
	}
}

func TestFailedImagePullKeepsPreviousCellFiles(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root, _, err := runFixture(t, func(plan *Plan) {
		plan.Kind = "preinstall"
		plan.Wipe = true
		plan.Runtime = &PreinstallRuntime{Image: "node:22", Startup: "node index.js"}
	}, nil)
	if err == nil {
		t.Fatal("failed image pull accepted")
	}
	data, err := os.ReadFile(filepath.Join(root, "world", "region.dat"))
	if err != nil || string(data) != "world" {
		t.Fatal("image failure changed Cell files")
	}
}

func TestPreinstallCommitsFilesAndPinnedImageTogether(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = pull ]; then exit 0; fi\necho ghcr.io/example/bot@sha256:" + strings.Repeat("a", 64) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root, result, err := runFixture(t, func(plan *Plan) {
		plan.Kind = "preinstall"
		plan.Wipe = true
		plan.Runtime = &PreinstallRuntime{Image: "ghcr.io/example/bot:1", Startup: "node app.js", SecretKeys: []string{"ENCRYPTION_KEY"}}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Startup != "node app.js" || !strings.Contains(result.Image, "@sha256:") {
		t.Fatal("runtime not pinned")
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); err != nil {
		t.Fatal("private configuration missing")
	}
	if _, err := os.Stat(filepath.Join(root, "world")); !os.IsNotExist(err) {
		t.Fatal("previous world was not replaced")
	}
	if data, err := os.ReadFile(filepath.Join(result.Backup, "original", "world", "region.dat")); err != nil || string(data) != "world" {
		t.Fatal("previous files not retained")
	}
}
