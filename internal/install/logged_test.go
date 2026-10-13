package install

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestLogWriterBoundsLongLinesAndTail(t *testing.T) {
	count := 0
	writer := &logWriter{log: func(line string) {
		count++
		if len(line) > 8192 {
			t.Fatal("unbounded line")
		}
	}}
	data := []byte(strings.Repeat("x", 100000) + "\nlast\r")
	n, err := writer.Write(data)
	writer.flush()
	if err != nil || n != len(data) || count < 2 || len(writer.tail) > 65536 {
		t.Fatal("invalid output bounds")
	}
	if !strings.HasSuffix(string(writer.tail), "last\r") {
		t.Fatal("lost tail")
	}
}

func TestRunLoggedStreamsBeforeProcessExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	// The child waits for a marker written by our callback. Buffering until exit would deadlock.
	marker := t.TempDir() + "/received"
	cmd := exec.Command("sh", "-c", `echo first; for i in 1 2 3 4 5; do test -f "$1" && { echo second >&2; exit 0; }; sleep 0.1; done; exit 1`, "sh", marker)
	var lines []string
	output, err := runLogged(cmd, func(line string) {
		lines = append(lines, line)
		if line == "first" {
			if err := os.WriteFile(marker, []byte("ok"), 0600); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil || len(lines) != 2 || !strings.Contains(string(output), "second") {
		t.Fatalf("not live: %v %v", err, lines)
	}
}
