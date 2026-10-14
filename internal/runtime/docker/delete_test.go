package docker

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDeletingAStoppedCellRemovesItsDockerContainer(t *testing.T) {
	removed := false
	runtime, _ := stopRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/json") {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"Id":"container","State":{"Running":false},"Config":{"Labels":{"hivepanel.worker":"true","hivepanel.cell_id":"cell"}}}`)
			return
		}
		if r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "/container") {
			removed = true
			w.WriteHeader(204)
			return
		}
		t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
	})
	if err := runtime.Delete("cell"); err != nil {
		t.Fatal(err)
	}
	if !removed || runtime.containers["cell"] != "" {
		t.Fatal("stopped container not deleted")
	}
}

func TestDeletionRefusesUnownedOrRunningContainers(t *testing.T) {
	for _, body := range []string{`{"Id":"container","State":{"Running":true},"Config":{"Labels":{"hivepanel.worker":"true","hivepanel.cell_id":"cell"}}}`, `{"Id":"container","State":{"Running":false},"Config":{"Labels":{}}}`} {
		runtime, _ := stopRuntime(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Error("unsafe removal attempted")
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		})
		if runtime.Delete("cell") == nil {
			t.Fatal("unsafe deletion accepted")
		}
	}
}
