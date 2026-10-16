package api

import (
	"hivepanel-worker/internal/filelease"
	"net/http"
)

func (h *Handler) FileLease(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := h.Manager.Get(r.PathValue("id"))
		if !ok {
			writeJSONStatus(w, 404, map[string]string{"message": "Cell not found"})
			return
		}
		release := filelease.Read(c.Dir)
		defer release()
		next.ServeHTTP(w, r)
	})
}
