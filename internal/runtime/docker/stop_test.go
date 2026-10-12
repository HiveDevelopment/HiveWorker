package docker

import (
	"github.com/docker/docker/client"
	hiveruntime "hivepanel-worker/internal/runtime"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type commandBuffer struct{ strings.Builder }

func (b *commandBuffer) Close() error { return nil }

func stopRuntime(t *testing.T, handler http.HandlerFunc) (*DockerRuntime, *commandBuffer) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	buffer := &commandBuffer{}
	return &DockerRuntime{client: cli, containers: map[string]string{"cell": "container"}, stdin: map[string]io.WriteCloser{"cell": buffer}}, buffer
}

func TestShutdownCommandGetsGraceBeforeSignal(t *testing.T) {
	var stops atomic.Int32
	runtime, buffer := stopRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wait") {
			time.Sleep(40 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"StatusCode":0}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stop") {
			stops.Add(1)
		}
		w.WriteHeader(204)
	})
	if err := runtime.StopConfigured("cell", hiveruntime.StopOptions{Command: "quit", Timeout: 1, Signal: "SIGINT"}); err != nil {
		t.Fatal(err)
	}
	if buffer.String() != "quit\n" || stops.Load() != 0 {
		t.Fatalf("command=%q stops=%d", buffer.String(), stops.Load())
	}
}

func TestSignalOnlyShutdownUsesConfiguredSignalAndTimeout(t *testing.T) {
	var stopped atomic.Bool
	runtime, buffer := stopRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/stop") {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		if r.URL.Query().Get("signal") != "SIGINT" || r.URL.Query().Get("t") != "45" {
			t.Errorf("unexpected options %s", r.URL.RawQuery)
		}
		stopped.Store(true)
		w.WriteHeader(204)
	})
	if err := runtime.StopConfigured("cell", hiveruntime.StopOptions{Timeout: 45, Signal: "SIGINT"}); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() || buffer.String() != "" {
		t.Fatal("signal-only shutdown wrote a console command")
	}
}

func TestMissingConsoleFallsBackToSignal(t *testing.T) {
	var stopped atomic.Bool
	runtime, _ := stopRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/stop") {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		stopped.Store(true)
		w.WriteHeader(204)
	})
	delete(runtime.stdin, "cell")
	if err := runtime.StopConfigured("cell", hiveruntime.StopOptions{Command: "quit", Timeout: 1, Signal: "SIGTERM"}); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() {
		t.Fatal("missing console prevented signal shutdown")
	}
}

func TestUnresponsiveCommandFallsBackAfterGrace(t *testing.T) {
	var stopped atomic.Bool
	runtime, _ := stopRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wait") {
			<-r.Context().Done()
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/stop") {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		stopped.Store(true)
		w.WriteHeader(204)
	})
	started := time.Now()
	if err := runtime.StopConfigured("cell", hiveruntime.StopOptions{Command: "quit", Timeout: 1, Signal: "SIGINT"}); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() || time.Since(started) < time.Second {
		t.Fatal("signal was sent before command grace elapsed")
	}
}
