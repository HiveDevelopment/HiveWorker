package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLegacyImageWithoutBashUsesPOSIXShell(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("/bin/sh", filepath.Join(dir, "sh")); err != nil {
		t.Fatal(err)
	}
	command := `printf '%s' 'literal $HOME; $(printf injected)'`
	args := shellCommand(command)
	cmd := exec.Command("/bin/sh", args[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh-only image failed: %v %s", err, output)
	}
	if string(output) != "literal $HOME; $(printf injected)" {
		t.Fatalf("launcher changed command quoting: %s", output)
	}
}
func TestLegacyImagePreservesBashWhenAvailable(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("Bash unavailable")
	}
	args := shellCommand(`[[ yes == yes ]] && printf 'bash-used'`)
	output, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil || string(output) != "bash-used" {
		t.Fatalf("Bash semantics lost: %v %s", err, output)
	}
}
func TestExplicitCombShellIsRespected(t *testing.T) {
	if !reflect.DeepEqual(shellCommand("node app.js", "sh"), []string{"sh", "-c", "node app.js"}) {
		t.Fatal("explicit sh ignored")
	}
	if !reflect.DeepEqual(shellCommand("echo ok", "bash"), []string{"bash", "-lc", "echo ok"}) {
		t.Fatal("explicit Bash ignored")
	}
}
