package api

import (
	"context"
	"encoding/json"
	"hivepanel-worker/internal/firewall"
	"net/http"
	"time"
)

func (h *Handler) CellFirewall(w http.ResponseWriter, r *http.Request) {
	if h.Firewall == nil {
		writeJSONStatus(w, 503, map[string]string{"message": "Firewall management requires a Linux Docker node with the supported iptables backend"})
		return
	}
	if _, ok := h.Manager.Get(r.PathValue("id")); !ok {
		writeJSONStatus(w, 404, map[string]string{"message": "Cell not found"})
		return
	}
	writeJSON(w, h.Firewall.Status(r.PathValue("id")))
}
func (h *Handler) SetCellFirewall(w http.ResponseWriter, r *http.Request) {
	if h.Firewall == nil {
		writeJSONStatus(w, 503, map[string]string{"message": "Firewall management is unavailable on this node"})
		return
	}
	var policy firewall.Policy
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": "Invalid firewall rules"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	err := h.Manager.WithCellDefinition(r.PathValue("id"), func(_ []int) error { return h.Firewall.Set(ctx, r.PathValue("id"), policy) })
	if err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	writeJSON(w, h.Firewall.Status(r.PathValue("id")))
}
func (h *Handler) CellInstances(w http.ResponseWriter, r *http.Request) {
	data, err := h.Manager.InstanceList(r.PathValue("id"))
	if err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	writeJSON(w, data)
}
func (h *Handler) CreateCellInstance(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name  string `json:"name"`
		Empty bool   `json:"empty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		writeJSONStatus(w, 422, map[string]string{"message": "Invalid instance settings"})
		return
	}
	if r.URL.Query().Get("wait") == "1" {
		entry, err := h.Manager.CreateInstance(r.PathValue("id"), request.Name, request.Empty)
		if err != nil {
			writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
			return
		}
		writeJSONStatus(w, 201, map[string]string{"id": entry.ID, "name": entry.Name})
		return
	}
	job, err := h.Manager.StartCreateInstance(r.PathValue("id"), request.Name, request.Empty)
	if err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	writeJSONStatus(w, 202, job)
}
func (h *Handler) ActivateCellInstance(w http.ResponseWriter, r *http.Request) {
	c, err := h.Manager.ActivateInstance(r.PathValue("id"), r.PathValue("instance"), r.Header.Get("X-Hive-Operation"))
	if err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	writeJSON(w, map[string]any{"id": c.ID, "comb": c.Comb, "comb_data": c.CombData, "variables": c.Variables, "docker": c.Docker, "startup": c.Startup, "install_status": c.InstallStatus})
}
func (h *Handler) DeleteCellInstance(w http.ResponseWriter, r *http.Request) {
	if err := h.Manager.DeleteInstance(r.PathValue("id"), r.PathValue("instance")); err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	writeJSON(w, map[string]string{"message": "Instance deleted"})
}

func (h *Handler) operationRequest(w http.ResponseWriter, r *http.Request, end bool) {
	var request struct {
		Token string `json:"token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		writeJSONStatus(w, 422, map[string]string{"message": "Invalid resource operation"})
		return
	}
	var err error
	if end {
		err = h.Manager.EndOperation(r.PathValue("id"), request.Token)
	} else {
		err = h.Manager.BeginOperation(r.PathValue("id"), request.Token)
	}
	if err != nil {
		writeJSONStatus(w, 409, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": "Resource operation updated"})
}
func (h *Handler) BeginCellOperation(w http.ResponseWriter, r *http.Request) {
	h.operationRequest(w, r, false)
}
func (h *Handler) EndCellOperation(w http.ResponseWriter, r *http.Request) {
	h.operationRequest(w, r, true)
}
