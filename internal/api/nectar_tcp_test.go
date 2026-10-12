package api

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/cell"
	"hivepanel-worker/internal/comb"
	hiveruntime "hivepanel-worker/internal/runtime"
)

type nectarTestRuntime struct{}

func (nectarTestRuntime) Start(hiveruntime.RuntimeCell, func(string), func()) error { return nil }
func (nectarTestRuntime) Stop(string) error                                         { return nil }
func (nectarTestRuntime) SendCommand(string, string) error                          { return nil }
func (nectarTestRuntime) Stats(string) (*hiveruntime.Stats, error)                  { return &hiveruntime.Stats{}, nil }
func (nectarTestRuntime) IsRunning(string) bool                                     { return false }
func (nectarTestRuntime) StartedAt(string) *time.Time                               { return nil }

func TestNectarAllocationTargets(t *testing.T) {
	assigned := []allocation.Allocation{{IP: "0.0.0.0", Port: 8192}, {IP: "::", Port: 8193}, {IP: "10.0.0.5", Port: 1234}}
	for _, input := range []struct {
		ip     string
		port   int
		target string
	}{{"0.0.0.0", 8192, "127.0.0.1:8192"}, {"::", 8193, "[::1]:8193"}, {"10.0.0.5", 1234, "10.0.0.5:1234"}} {
		actual, err := allocationTarget(input.ip, input.port, assigned)
		if err != nil || actual != input.target {
			t.Fatalf("target=%s error=%v", actual, err)
		}
	}
	for _, input := range []allocation.Allocation{{IP: "127.0.0.1", Port: 22}, {IP: "0.0.0.0", Port: 22}, {IP: "example.com", Port: 8192}} {
		if _, err := allocationTarget(input.IP, input.Port, assigned); err == nil {
			t.Fatal("unassigned target allowed")
		}
	}
}

func TestNectarTCPHandshakeScopeAndClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = io.WriteString(conn, "HELLO challenge\n")
		if err == nil {
			_, err = bufio.NewReader(conn).ReadString('\n')
		}
		if err == nil {
			_, err = io.WriteString(conn, `{"status":"ok"}`)
		}
		done <- err
	}()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "cells"), 0755)
	data, _ := json.Marshal(cell.Cell{ID: "test", Name: "test", Allocation: allocation.Allocation{IP: "127.0.0.1", Port: port}})
	if err := os.WriteFile(filepath.Join(dir, "cells", "test.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	manager := cell.NewManager(dir, t.TempDir(), comb.NewManager(t.TempDir()), nectarTestRuntime{}, allocation.NewManager(nil), nil)
	if err := manager.Load(); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Manager: manager}
	call := func(input nectarTCPRequest) (int, map[string]any) {
		data, _ := json.Marshal(input)
		request := httptest.NewRequest("POST", "/cells/test/network/tcp", bytes.NewReader(data))
		request.SetPathValue("id", "test")
		response := httptest.NewRecorder()
		h.NectarTCP(response, request)
		var result map[string]any
		json.Unmarshal(response.Body.Bytes(), &result)
		return response.Code, result
	}
	input := nectarTCPRequest{Action: "open", Scope: strings.Repeat("a", 64), IP: "127.0.0.1", Port: port, ReadLine: true}
	status, result := call(input)
	if status != 200 {
		t.Fatalf("open=%d", status)
	}
	greeting, _ := base64.StdEncoding.DecodeString(result["data_base64"].(string))
	if string(greeting) != "HELLO challenge\n" {
		t.Fatal("bad greeting")
	}
	input.SessionID = result["session_id"].(string)
	input.Action = "exchange"
	input.Scope = strings.Repeat("b", 64)
	if status, _ := call(input); status != 403 {
		t.Fatal("cross-scope session allowed")
	}
	input.Scope = strings.Repeat("a", 64)
	input.ReadLine = false
	input.ReadJSON = true
	input.Data = base64.StdEncoding.EncodeToString([]byte("packet\n"))
	status, result = call(input)
	if status != 200 {
		t.Fatalf("exchange=%d", status)
	}
	data, _ = base64.StdEncoding.DecodeString(result["data_base64"].(string))
	if string(data) != `{"status":"ok"}` {
		t.Fatal("JSON framing failed")
	}
	input.Action = "close"
	if status, _ := call(input); status != 200 {
		t.Fatal("close failed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if status, _ := call(input); status != 403 {
		t.Fatal("closed session reused")
	}
}
