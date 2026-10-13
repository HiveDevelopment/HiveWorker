package api

import (
	"encoding/json"
	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/cell"
	"hivepanel-worker/internal/comb"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestOverviewReturnsOnlyRequestedCellWithRuntimeStats(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cells"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"visible", "other"} {
		data, _ := json.Marshal(cell.Cell{ID: id, Name: id, Dir: t.TempDir()})
		if err := os.WriteFile(filepath.Join(dir, "cells", id+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager := cell.NewManager(dir, t.TempDir(), comb.NewManager(t.TempDir()), nectarTestRuntime{}, allocation.NewManager(nil), nil)
	if err := manager.Load(); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{Manager: manager}
	response := httptest.NewRecorder()
	handler.CellOverview(response, httptest.NewRequest("GET", "/cells/overview?ids=visible", nil))
	var result struct {
		Cells []struct {
			ID     string         `json:"id"`
			Status string         `json:"status"`
			Stats  map[string]any `json:"stats"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Cells) != 1 || result.Cells[0].ID != "visible" || result.Cells[0].Status != "offline" {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
	if running, ok := result.Cells[0].Stats["running"].(bool); !ok || running {
		t.Fatal("missing runtime state")
	}
}
