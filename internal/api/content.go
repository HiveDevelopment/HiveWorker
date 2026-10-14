package api

import (
	"encoding/json"
	"hivepanel-worker/internal/minecraft"
	"net/http"
	"strings"
)

func (h *Handler) InstallContent(w http.ResponseWriter, r *http.Request) {
	var plan minecraft.Plan
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
		writeJSONStatus(w, 400, map[string]string{"message": "Invalid install plan"})
		return
	}
	if plan.Kind == "preinstall" && h.Config.Runtime.Type != "docker" {
		writeJSONStatus(w, 409, map[string]string{"message": "Preinstalls require the Docker runtime"})
		return
	}
	registry := strings.TrimRight(h.Config.Registry.URL, "/")
	if registry == "" {
		registry = strings.TrimRight(h.Config.Minecraft.RegistryURL, "/")
	}
	if registry == "" {
		registry = "https://registry.hivepanel.dev"
	}
	job, err := h.Manager.StartContentInstall(r.PathValue("id"), registry, plan)
	if err != nil {
		writeJSONStatus(w, 409, map[string]string{"message": err.Error()})
		return
	}
	writeJSONStatus(w, 202, job)
}
func (h *Handler) ContentStatus(w http.ResponseWriter, r *http.Request) {
	job, err := h.Manager.ContentStatus(r.PathValue("id"))
	if err != nil {
		writeJSONStatus(w, 404, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, job)
}
