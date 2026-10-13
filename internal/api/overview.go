package api

import (
	"hivepanel-worker/internal/cell"
	"net/http"
	"strings"
	"sync"
)

// Collect Docker measurements concurrently without spawning unbounded requests.
func (h *Handler) CellOverview(w http.ResponseWriter, r *http.Request) {
	var cells []*cell.Cell
	if ids := r.URL.Query().Get("ids"); ids != "" {
		requested := strings.Split(ids, ",")
		if len(requested) > 500 {
			http.Error(w, "too many Cells", http.StatusBadRequest)
			return
		}
		for _, id := range requested {
			if current, ok := h.Manager.Get(id); ok {
				cells = append(cells, current)
			}
		}
	} else {
		cells = h.Manager.List()
	}
	results := make([]map[string]any, len(cells))
	limit := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for index, gameCell := range cells {
		select {
		case limit <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		wait.Add(1)
		go func(index int, id string) {
			defer wait.Done()
			defer func() { <-limit }()
			current, err := h.Manager.Status(id)
			if err != nil {
				results[index] = map[string]any{"id": id, "status": "unknown"}
				return
			}
			stats, err := h.Manager.Stats(id)
			if err != nil {
				results[index] = map[string]any{"id": id, "status": "unknown"}
				return
			}
			results[index] = map[string]any{"id": id, "name": current.Name, "comb": current.Comb, "allocation": current.Allocation, "limits": current.Limits, "status": "offline", "stats": stats}
			if stats.Running {
				results[index]["status"] = "running"
			}
		}(index, gameCell.ID)
	}
	wait.Wait()
	writeJSON(w, map[string]any{"cells": results})
}
