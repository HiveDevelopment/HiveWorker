//go:build !windows

package process

import (
	hiveruntime "hivepanel-worker/internal/runtime"
	"testing"
	"time"
)

func TestConsoleShutdownExitsWithoutImmediateKill(t *testing.T) {
	runtime := New()
	exited := make(chan struct{})
	cell := hiveruntime.RuntimeCell{ID: "test", InstanceDir: t.TempDir(), Command: `while IFS= read -r command; do if [ "$command" = "quit" ]; then exit 0; fi; done`}
	if err := runtime.Start(cell, func(string) {}, func() { close(exited) }); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if runtime.IsRunning("test") {
			_ = runtime.Stop("test")
		}
	}()
	if err := runtime.StopConfigured("test", hiveruntime.StopOptions{Command: "quit", Timeout: 1, Signal: "SIGTERM"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("shutdown command did not exit")
	}
	if runtime.IsRunning("test") {
		t.Fatal("exited process is still marked running")
	}
}
