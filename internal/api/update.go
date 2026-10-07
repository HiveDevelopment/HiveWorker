package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

type WorkerUpdateRequest struct {
	Version string `json:"version"`
}

func (h *Handler) UpdateWorker(
	w http.ResponseWriter,
	r *http.Request,
) {
	if h.Updater == nil {
		http.Error(
			w,
			"Worker updater is not available",
			http.StatusServiceUnavailable,
		)
		return
	}

	var request WorkerUpdateRequest

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&request); err != nil {
		http.Error(
			w,
			"invalid json body",
			http.StatusBadRequest,
		)
		return
	}

	request.Version = strings.TrimSpace(request.Version)

	if request.Version == "" {
		http.Error(
			w,
			"version is required",
			http.StatusBadRequest,
		)
		return
	}

	if err := h.Updater.Start(request.Version); err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusConflict,
		)
		return
	}

	writeJSONStatus(
		w,
		http.StatusAccepted,
		h.Updater.Status(),
	)
}

func (h *Handler) WorkerUpdateStatus(
	w http.ResponseWriter,
	r *http.Request,
) {
	if h.Updater == nil {
		http.Error(
			w,
			"Worker updater is not available",
			http.StatusServiceUnavailable,
		)
		return
	}

	writeJSON(w, h.Updater.Status())
}
