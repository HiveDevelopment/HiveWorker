package api

import (
	"context"
	"encoding/json"
	"errors"
	"hivepanel-worker/internal/webproxy"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"
)

func (h *Handler) proxyAvailable(w http.ResponseWriter) bool {
	if h.WebProxy == nil || h.Config.Runtime.Type != "docker" {
		writeJSONStatus(w, 503, map[string]string{"message": "Web proxies require a configured Docker Worker"})
		return false
	}
	return true
}
func (h *Handler) NodeWebProxy(w http.ResponseWriter, r *http.Request) {
	if !h.proxyAvailable(w) {
		return
	}
	writeJSON(w, h.WebProxy.Status())
}
func (h *Handler) ConfigureWebProxy(w http.ResponseWriter, r *http.Request) {
	if !h.proxyAvailable(w) {
		return
	}
	var c webproxy.Config
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if json.NewDecoder(r.Body).Decode(&c) != nil {
		writeJSONStatus(w, 400, map[string]string{"message": "Invalid node proxy settings"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	err := h.Manager.WithDefinitionLock(func() error {
		ports := []int{}
		for _, cell := range h.Manager.List() {
			ports = append(ports, cell.Allocation.Port)
			for _, allocation := range cell.AdditionalAllocations {
				ports = append(ports, allocation.Port)
			}
			for _, allocation := range cell.ActiveAllocations {
				ports = append(ports, allocation.Port)
			}
		}
		return h.WebProxy.Configure(ctx, c, ports)
	})
	if err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, h.WebProxy.Status())
}
func (h *Handler) CellDomains(w http.ResponseWriter, r *http.Request) {
	if !h.proxyAvailable(w) {
		return
	}
	if _, ok := h.Manager.Get(r.PathValue("id")); !ok {
		writeJSONStatus(w, 404, map[string]string{"message": "Cell not found"})
		return
	}
	writeJSON(w, h.WebProxy.PublicRoutes(r.PathValue("id")))
}
func (h *Handler) SaveDomain(w http.ResponseWriter, r *http.Request) {
	if !h.proxyAvailable(w) {
		return
	}
	var route webproxy.Route
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if json.NewDecoder(r.Body).Decode(&route) != nil {
		writeJSONStatus(w, 400, map[string]string{"message": "Invalid domain route"})
		return
	}
	route.CellID = r.PathValue("id")
	route.Verified = false
	route.CertificateState = ""
	route.Error = ""
	route.VerificationToken = ""
	route.CertificateExpires = nil
	var saved webproxy.Route
	err := h.Manager.WithCellDefinition(route.CellID, func(ports []int) error {
		valid := false
		for _, port := range ports {
			if port == route.Port {
				valid = true
			}
		}
		if !valid {
			return errors.New("choose an allocation assigned to this Cell")
		}
		var err error
		saved, err = h.WebProxy.Put(r.Context(), route)
		return err
	})
	if err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	if saved.Verified && saved.HTTPS && (saved.CertificateState == "none" || saved.CertificateState == "failed") {
		_ = h.WebProxy.RequestCertificate(saved.CellID, saved.ID)
	}
	writeJSON(w, map[string]any{"route": saved, "dns": h.WebProxy.DNSInstructions(saved)})
}
func (h *Handler) VerifyDomain(w http.ResponseWriter, r *http.Request) {
	if !h.proxyAvailable(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	route, err := h.WebProxy.Verify(ctx, r.PathValue("id"), r.PathValue("route"))
	if err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	if route.HTTPS {
		_ = h.WebProxy.RequestCertificate(route.CellID, route.ID)
	}
	writeJSON(w, map[string]any{"route": route, "dns": h.WebProxy.DNSInstructions(route)})
}
func (h *Handler) DomainCertificate(w http.ResponseWriter, r *http.Request) {
	if !h.proxyAvailable(w) {
		return
	}
	if err := h.WebProxy.RequestCertificate(r.PathValue("id"), r.PathValue("route")); err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	writeJSONStatus(w, 202, map[string]string{"message": "Certificate request started"})
}
func (h *Handler) DeleteDomain(w http.ResponseWriter, r *http.Request) {
	if !h.proxyAvailable(w) {
		return
	}
	if err := h.WebProxy.Delete(r.Context(), r.PathValue("id"), r.PathValue("route")); err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	writeJSON(w, map[string]string{"message": "Domain route removed"})
}
func (h *Handler) AdminDeleteDomain(w http.ResponseWriter, r *http.Request) {
	if !h.proxyAvailable(w) {
		return
	}
	if err := h.WebProxy.Delete(r.Context(), "", r.PathValue("route")); err != nil {
		writeJSONStatus(w, 422, map[string]string{"message": publicProxyError(err)})
		return
	}
	writeJSON(w, map[string]string{"message": "Domain route removed"})
}

func publicProxyError(err error) string {
	var pathError *os.PathError
	var commandError *exec.ExitError
	if errors.As(err, &pathError) || errors.As(err, &commandError) {
		log.Printf("Node proxy operation failed: %v", err)
		return "The node proxy could not apply this change. Ask an administrator to check its storage, permissions and Worker journal."
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "The node proxy operation timed out. Refresh its status before trying again."
	}
	return err.Error()
}
