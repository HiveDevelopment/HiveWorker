package panel

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"hivepanel-worker/internal/config"
	nodestats "hivepanel-worker/internal/node"
	"hivepanel-worker/internal/version"
)

type HeartbeatPayload struct {
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	Platform string `json:"platform"`
	Stats    any    `json:"stats"`
}

type heartbeatResponse struct {
	OK            bool            `json:"ok"`
	NodeID        string          `json:"node_id"`
	Timestamp     string          `json:"timestamp"`
	Configuration json.RawMessage `json:"configuration"`
}

func StartHeartbeat(cfg config.Config) {
	if cfg.Panel.URL == "" || cfg.Worker.Token == "" || cfg.Node.ID == "" {
		log.Println("Panel heartbeat disabled: missing panel URL, worker token or node ID")
		return
	}

	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		if sendHeartbeat(cfg) {
			return
		}

		for range ticker.C {
			if sendHeartbeat(cfg) {
				return
			}
		}
	}()
}

// sendHeartbeat returns true when a new configuration was persisted and a
// Worker restart has been scheduled.
func sendHeartbeat(cfg config.Config) bool {
	hostname, _ := os.Hostname()

	stats, err := nodestats.GetStats(cfg.Paths.Data)
	if err != nil {
		log.Println("Failed to collect node stats:", err)
		return false
	}

	payload := HeartbeatPayload{
		Version:  version.Version,
		Hostname: hostname,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Stats:    stats,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Println("Failed to encode heartbeat:", err)
		return false
	}

	request, err := http.NewRequest(
		http.MethodPost,
		trimSlash(cfg.Panel.URL)+"/api/worker/heartbeat",
		bytes.NewReader(body),
	)
	if err != nil {
		log.Println("Failed to build heartbeat request:", err)
		return false
	}

	request.Header.Set("Authorization", "Bearer "+cfg.Worker.Token)
	request.Header.Set("X-Hive-Node", cfg.Node.ID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	response, err := client.Do(request)
	if err != nil {
		log.Println("Failed to send heartbeat:", err)
		return false
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK ||
		response.StatusCode >= http.StatusMultipleChoices {
		log.Println("Heartbeat failed with HTTP status:", response.StatusCode)
		return false
	}

	var result heartbeatResponse

	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		log.Println("Failed to decode heartbeat response:", err)
		return false
	}

	changed, err := config.ApplyRemoteConfiguration(
		cfg,
		result.Configuration,
	)
	if err != nil {
		log.Println("Failed to apply Panel configuration:", err)
		return false
	}

	if !changed {
		return false
	}

	log.Println("Worker configuration changed; restarting HiveWorker...")

	// Give the heartbeat request time to finish cleanly before exiting.
	// The standard HivePanel systemd unit uses Restart=always, so systemd
	// immediately starts the Worker again with the newly persisted config.
	go func() {
		time.Sleep(time.Second)
		os.Exit(0)
	}()

	return true
}

func trimSlash(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}

	return value
}
