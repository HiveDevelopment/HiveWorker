package minecraft

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecipeFileStepsKeepShellCharactersLiteral(t *testing.T) {
	root := t.TempDir()
	steps := []RecipeStep{
		{Type: "mkdir", With: map[string]string{"path": "source dir"}},
		{Type: "write_file", With: map[string]string{"path": "source dir/config.txt", "content": "$(touch injected) 'quoted'"}},
		{Type: "copy", With: map[string]string{"source": "source dir", "target": "output dir"}},
		{Type: "move", With: map[string]string{"source": "output dir/config.txt", "target": "output dir/moved.txt"}},
		{Type: "delete", With: map[string]string{"path": "source dir"}},
	}
	script := "set -eu\n"
	for _, step := range steps {
		body, err := stepScript(step)
		if err != nil {
			t.Fatal(err)
		}
		script += body
	}
	command := exec.Command("sh", "-ec", script)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	content, err := os.ReadFile(filepath.Join(root, "output dir", "moved.txt"))
	if err != nil || string(content) != "$(touch injected) 'quoted'" {
		t.Fatalf("content changed: %s %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(root, "injected")); !os.IsNotExist(err) {
		t.Fatal("shell interpolation executed")
	}
}

func TestRecipePathsAndGitOptionsAreValidated(t *testing.T) {
	for _, value := range []string{"../outside", "/tmp/outside", ".hivepanel/lock.json"} {
		if _, err := stepScript(RecipeStep{Type: "delete", With: map[string]string{"path": value}}); err == nil {
			t.Fatal("unsafe recipe path accepted")
		}
	}
	if _, err := stepScript(RecipeStep{Type: "git", With: map[string]string{"repository": "file:///etc", "target": "source", "ref": "main"}}); err == nil {
		t.Fatal("local Git source accepted")
	}
	script, err := stepScript(RecipeStep{Type: "git", With: map[string]string{"repository": "https://github.com/example/bot.git", "target": "source", "ref": "{{SOURCE_REF}}"}})
	if err != nil || !strings.Contains(script, `--end-of-options "${SOURCE_REF}^{commit}"`) {
		t.Fatalf("unsafe Git reference handling: %s %v", script, err)
	}
}

func TestDependencyLinksStayInsideStaging(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "node_modules", ".bin"), 0750)
	os.MkdirAll(filepath.Join(root, "node_modules", "package"), 0750)
	os.WriteFile(filepath.Join(root, "node_modules", "package", "tool"), []byte("tool"), 0640)
	link := filepath.Join(root, "node_modules", ".bin", "tool")
	os.Symlink("../package/tool", link)
	if err := safeStagedLink(root, link); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "escape")
	os.Symlink("/etc/passwd", outside)
	if safeStagedLink(root, outside) == nil {
		t.Fatal("absolute link accepted")
	}
}
