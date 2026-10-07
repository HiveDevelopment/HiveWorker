package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"hivepanel-worker/internal/backup"
	"hivepanel-worker/internal/cell"
	"hivepanel-worker/internal/comb"
	"hivepanel-worker/internal/config"
	"hivepanel-worker/internal/files"
	"hivepanel-worker/internal/importer"
	"hivepanel-worker/internal/migrationdetect"
	"hivepanel-worker/internal/node"
	"hivepanel-worker/internal/players"
	"hivepanel-worker/internal/updater"
)

type Handler struct {
	Config       config.Config
	Manager      *cell.Manager
	CombManager  *comb.Manager
	BackupMounts *backup.MountService
	Updater      *updater.Manager
}

type CommandRequest struct {
	Command string `json:"command"`
}

type ConsoleSession struct {
	CellID    string
	ExpiresAt time.Time
}

type PlayerActionRequest struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type FileWriteRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type FilePathRequest struct {
	Path string `json:"path"`
}

type FileRenameRequest struct {
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
}

type FileRestoreRequest struct {
	Path string `json:"path"`
}

type CreateFileRequest struct {
	Path string `json:"path"`
}

type UploadURLRequest struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

type BackupExtractRequest struct {
	Path string `json:"path"`
}

type ConfigWriteRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type RconRequest struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	Password string `json:"password"`
}

type ImporterRequest struct {
	Protocol             string         `json:"protocol"`
	Host                 string         `json:"host"`
	Port                 int            `json:"port"`
	Username             string         `json:"username"`
	AuthType             string         `json:"auth_type"`
	Password             string         `json:"password"`
	PrivateKey           string         `json:"private_key"`
	PrivateKeyPassphrase string         `json:"private_key_passphrase"`
	RemotePath           string         `json:"remote_path"`
	Options              map[string]any `json:"options"`
}

type MigrationSourceDetectionRequest struct {
	UUIDs          []string `json:"uuids"`
	ConfiguredPath string   `json:"configured_path"`
}

type ImportProgress struct {
	Running bool   `json:"running"`
	Stage   string `json:"stage"`
	Percent int    `json:"percent"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

type LockCellRequest struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

var importProgressStore = struct {
	sync.RWMutex
	items map[string]ImportProgress
}{
	items: map[string]ImportProgress{},
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func invalidCellID(id string) bool {
	return id == "" || id == "null" || id == "undefined"
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"status": "ok",
		"name":   "HivePanel Daemon",
	})
}

func (h *Handler) DetectMigrationSource(w http.ResponseWriter, r *http.Request) {
	var request MigrationSourceDetectionRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if len(request.UUIDs) == 0 {
		http.Error(w, "at least one source server uuid is required", http.StatusBadRequest)
		return
	}

	result := migrationdetect.Detect(
		migrationdetect.Request{
			UUIDs:          request.UUIDs,
			ConfiguredPath: request.ConfiguredPath,
		},
	)

	writeJSON(w, result)
}

func (h *Handler) NodeStats(w http.ResponseWriter, r *http.Request) {
	stats, err := node.GetStats(".")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cells := h.Manager.List()

	stats.Cells.Total = len(cells)

	for _, gameCell := range cells {
		if gameCell.Status == "running" {
			stats.Cells.Running++
		}

		cellStats, err := h.Manager.Stats(gameCell.ID)
		if err != nil {
			continue
		}

		stats.Cells.CPUUsed += cellStats.CPU
		stats.Cells.MemoryUsedGB += cellStats.MemoryMB / 1024
		stats.Cells.DiskUsedGB += float64(cellStats.DiskBytes) / 1024 / 1024 / 1024
	}

	stats.Cells.CPUUsed = roundFloat(stats.Cells.CPUUsed)
	stats.Cells.MemoryUsedGB = roundFloat(stats.Cells.MemoryUsedGB)
	stats.Cells.DiskUsedGB = roundFloat(stats.Cells.DiskUsedGB)

	writeJSON(w, stats)
}

func (h *Handler) ListCells(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.Manager.List())
}

func (h *Handler) CellStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	gameCell, err := h.Manager.Status(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	writeJSON(w, gameCell)
}

func (h *Handler) ensurePowerAllowed(w http.ResponseWriter, id string) bool {
	if err := h.Manager.EnsurePowerAllowed(id); err != nil {
		http.Error(w, err.Error(), http.StatusLocked)
		return false
	}

	return true
}

func (h *Handler) ensureConsoleAllowed(w http.ResponseWriter, id string) bool {
	if err := h.Manager.EnsureConsoleAllowed(id); err != nil {
		http.Error(w, err.Error(), http.StatusLocked)
		return false
	}

	return true
}

func (h *Handler) ensureFilesAllowed(w http.ResponseWriter, id string) bool {
	if err := h.Manager.EnsureFilesAllowed(id); err != nil {
		http.Error(w, err.Error(), http.StatusLocked)
		return false
	}

	return true
}

func (h *Handler) ensureBackupsAllowed(w http.ResponseWriter, id string) bool {
	if err := h.Manager.EnsureBackupsAllowed(id); err != nil {
		http.Error(w, err.Error(), http.StatusLocked)
		return false
	}

	return true
}

func (h *Handler) ensureSettingsAllowed(w http.ResponseWriter, id string) bool {
	if err := h.Manager.EnsureSettingsAllowed(id); err != nil {
		http.Error(w, err.Error(), http.StatusLocked)
		return false
	}

	return true
}

func (h *Handler) ensureImporterAllowed(w http.ResponseWriter, id string) bool {
	if err := h.Manager.EnsureImporterAllowed(id); err != nil {
		http.Error(w, err.Error(), http.StatusLocked)
		return false
	}

	return true
}

func (h *Handler) StartCell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensurePowerAllowed(w, id) {
		return
	}

	_ = h.Manager.ClearConsole(id)

	if err := h.Manager.Start(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "cell started",
		"id":      id,
	})
}

func (h *Handler) StopCell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensurePowerAllowed(w, id) {
		return
	}

	_ = h.Manager.ClearConsole(id)

	if err := h.Manager.Stop(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]string{
		"id":      id,
		"message": "cell stopping",
	})
}

func (h *Handler) CellConsole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	lines, err := h.Manager.Console(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"id":    id,
		"lines": lines,
	})
}

func (h *Handler) CreateConsoleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureConsoleAllowed(w, id) {
		return
	}

	token, err := h.Manager.CreateConsoleSession(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"token":      token,
		"expires_in": 30,
	})
}

func (h *Handler) SendCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureConsoleAllowed(w, id) {
		return
	}

	var request CommandRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if request.Command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}

	if err := h.Manager.SendCommand(id, request.Command); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "command sent",
		"id":      id,
		"command": request.Command,
	})
}

func (h *Handler) ListFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	page := parsePositiveInt(
		r.URL.Query().Get("page"),
		1,
	)

	perPage := parsePositiveInt(
		r.URL.Query().Get("per_page"),
		files.DefaultPageSize,
	)

	if perPage > files.MaxPageSize {
		perPage = files.MaxPageSize
	}

	result, err := files.List(
		gameServer.Dir,
		path,
		page,
		perPage,
	)

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"id":         id,
		"path":       result.Path,
		"files":      result.Files,
		"pagination": result.Pagination,
	})
}

func parsePositiveInt(value string, fallback int) int {
	if value == "" {
		return fallback
	}

	number, err := strconv.Atoi(value)
	if err != nil || number < 1 {
		return fallback
	}

	return number
}

func (h *Handler) ReadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	content, err := files.Read(gameServer.Dir, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"id":      id,
		"path":    path,
		"content": content,
	})
}

func (h *Handler) WriteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureFilesAllowed(w, id) {
		return
	}

	var request FileWriteRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := files.Write(gameServer.Dir, request.Path, request.Content); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "file written",
		"path":    request.Path,
	})
}

func (h *Handler) DeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureFilesAllowed(w, id) {
		return
	}

	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := files.MoveToRecycleBin(gameServer.Dir, path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "file moved to recycle bin",
		"path":    path,
	})
}

func (h *Handler) RestoreFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureFilesAllowed(w, id) {
		return
	}

	var request FileRestoreRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if request.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := files.RestoreFromRecycleBin(gameServer.Dir, request.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "file restored",
		"path":    request.Path,
	})
}

func (h *Handler) PermanentDeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureFilesAllowed(w, id) {
		return
	}

	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := files.PermanentDeleteFromRecycleBin(gameServer.Dir, path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "file permanently deleted",
		"path":    path,
	})
}

func (h *Handler) CreateFolder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureFilesAllowed(w, id) {
		return
	}

	var request FilePathRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := files.CreateFolder(gameServer.Dir, request.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "folder created",
		"path":    request.Path,
	})
}

func (h *Handler) RenameFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureFilesAllowed(w, id) {
		return
	}

	var request FileRenameRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := files.Rename(gameServer.Dir, request.OldPath, request.NewPath); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message":  "file renamed",
		"old_path": request.OldPath,
		"new_path": request.NewPath,
	})
}

func (h *Handler) UploadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureFilesAllowed(w, id) {
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := r.ParseMultipartForm(512 << 20); err != nil {
		http.Error(w, "failed to parse upload", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file field is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "failed to read uploaded file", http.StatusInternalServerError)
		return
	}

	relativePath := r.FormValue("relative_path")
	if relativePath == "" {
		relativePath = header.Filename
	}

	targetPath := filepath.ToSlash(filepath.Join(path, relativePath))

	if err := files.WriteBytes(gameServer.Dir, targetPath, data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "file uploaded",
		"path":    targetPath,
		"size":    len(data),
	})
}

func (h *Handler) DownloadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	fullPath, err := files.DownloadPath(gameServer.Dir, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(fullPath)+"\"")
	http.ServeFile(w, r, fullPath)
}

func (h *Handler) CreateFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureFilesAllowed(w, id) {
		return
	}

	var request CreateFileRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if request.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := files.CreateFile(gameServer.Dir, request.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "file created",
		"path":    request.Path,
	})
}

func (h *Handler) UploadFromURL(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureFilesAllowed(w, id) {
		return
	}

	var request UploadURLRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if request.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	if request.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := files.UploadFromURL(gameServer.Dir, request.Path, request.URL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "file uploaded from url",
		"path":    request.Path,
	})
}

func (h *Handler) ConsoleWebSocket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	token := r.URL.Query().Get("token")

	if token == "" {
		http.Error(w, "console token is required", http.StatusUnauthorized)
		return
	}

	if !h.Manager.ValidateConsoleSession(id, token) {
		http.Error(w, "invalid or expired console token", http.StatusUnauthorized)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	defer func() {
		_ = conn.Close()
	}()

	includeHistory := r.URL.Query().Get("tail") != "0"

	ch, err := h.Manager.SubscribeWithHistory(id, includeHistory)
	if err != nil {
		_ = conn.WriteJSON(map[string]any{
			"type":    "error",
			"message": err.Error(),
		})

		return
	}

	defer func() {
		h.Manager.Unsubscribe(id, ch)
	}()

	if err := conn.WriteJSON(map[string]any{
		"type": "console",
		"line": "container@hivepanel~ Console websocket connected.",
	}); err != nil {
		return
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			var msg map[string]string

			if err := conn.ReadJSON(&msg); err != nil {
				return
			}

			if msg["type"] == "command" {
				command := msg["command"]

				if command == "" {
					continue
				}

				if err := h.Manager.SendCommand(id, command); err != nil {
					_ = conn.WriteJSON(map[string]any{
						"type":    "error",
						"message": err.Error(),
					})
				}
			}
		}
	}()

	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return
			}

			if err := conn.WriteJSON(map[string]any{
				"type":      "console",
				"line":      line.Message,
				"timestamp": line.Timestamp,
			}); err != nil {
				return
			}

		case <-done:
			return
		}
	}
}

func (h *Handler) CellStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	stats, err := h.Manager.Stats(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, stats)
}

func (h *Handler) CreateCell(w http.ResponseWriter, r *http.Request) {
	var request cell.CreateCellRequest

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}

	gameServer, err := h.Manager.Create(request)
	if err != nil {
		status := http.StatusBadRequest

		switch err.Error() {
		case "cell already exists", "cell directory already exists":
			status = http.StatusConflict
		}

		http.Error(w, err.Error(), status)
		return
	}

	writeJSON(w, gameServer)
}

func (h *Handler) InstallCell(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))

	if invalidCellID(id) {
		http.Error(
			w,
			"invalid cell id",
			http.StatusBadRequest,
		)
		return
	}

	if err := h.Manager.StartInstall(id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	writeJSONStatus(
		w,
		http.StatusAccepted,
		map[string]any{
			"message": "install started",
			"id":      id,
		},
	)
}

func (h *Handler) CellInstallStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	status, installError, err := h.Manager.InstallState(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	writeJSON(w, map[string]any{
		"status": status,
		"error":  installError,
	})
}

func (h *Handler) ReinstallCell(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))

	if invalidCellID(id) {
		http.Error(
			w,
			"invalid cell id",
			http.StatusBadRequest,
		)
		return
	}

	if err := h.Manager.Reinstall(id); err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusBadRequest,
		)
		return
	}

	writeJSON(w, map[string]any{
		"message": "cell prepared for reinstall",
		"id":      id,
	})
}

func (h *Handler) UpdateCellDefinition(
	w http.ResponseWriter,
	r *http.Request,
) {
	id := strings.TrimSpace(r.PathValue("id"))

	if invalidCellID(id) {
		http.Error(
			w,
			"invalid cell id",
			http.StatusBadRequest,
		)
		return
	}

	var request cell.UpdateCellDefinitionRequest

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

	gameServer, err := h.Manager.UpdateDefinition(
		id,
		request,
	)
	if err != nil {
		status := http.StatusBadRequest

		switch err.Error() {
		case "cell not found":
			status = http.StatusNotFound

		case "cell must be stopped before changing its definition":
			status = http.StatusConflict
		}

		http.Error(
			w,
			err.Error(),
			status,
		)
		return
	}

	writeJSON(w, gameServer)
}

func (h *Handler) DeleteCell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if err := h.Manager.Delete(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "cell deleted",
		"id":      id,
	})
}

func (h *Handler) ListCombs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.CombManager.List())
}

func (h *Handler) GetComb(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	comb, exists := h.CombManager.Get(id)
	if !exists {
		http.Error(w, "comb not found", http.StatusNotFound)
		return
	}

	writeJSON(w, comb)
}

type createBackupRequest struct {
	BackupID     string   `json:"backup_id"`
	Name         string   `json:"name"`
	IgnoredFiles []string `json:"ignored_files"`
}

func (h *Handler) CreateBackup(w http.ResponseWriter, r *http.Request) {
	cellID := r.PathValue("id")

	if invalidCellID(cellID) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureBackupsAllowed(w, cellID) {
		return
	}

	var request createBackupRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	request.BackupID = strings.TrimSpace(request.BackupID)
	request.Name = strings.TrimSpace(request.Name)

	if request.BackupID == "" {
		http.Error(w, "backup_id is required", http.StatusBadRequest)
		return
	}

	if request.Name == "" {
		request.Name = request.BackupID
	}

	createdBackup, err := h.Manager.CreateBackup(
		cellID,
		request.BackupID,
		request.Name,
		request.IgnoredFiles,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSONStatus(
		w,
		http.StatusCreated,
		createdBackup,
	)
}

func (h *Handler) ListBackups(w http.ResponseWriter, r *http.Request) {
	cellID := r.PathValue("id")

	if invalidCellID(cellID) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureBackupsAllowed(w, cellID) {
		return
	}

	backups, err := h.Manager.ListBackups(cellID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, backups)
}

func (h *Handler) DeleteBackup(w http.ResponseWriter, r *http.Request) {
	cellID := r.PathValue("id")
	backupID := strings.TrimSpace(r.PathValue("backupID"))

	if invalidCellID(cellID) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureBackupsAllowed(w, cellID) {
		return
	}

	if backupID == "" {
		http.Error(w, "backup id is required", http.StatusBadRequest)
		return
	}

	if err := h.Manager.DeleteBackup(cellID, backupID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message":   "backup deleted",
		"id":        cellID,
		"backup_id": backupID,
	})
}

func (h *Handler) DownloadBackup(
	w http.ResponseWriter,
	r *http.Request,
) {
	cellID := r.PathValue("id")
	backupID := strings.TrimSpace(r.PathValue("backupID"))

	if invalidCellID(cellID) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureBackupsAllowed(w, cellID) {
		return
	}

	if backupID == "" {
		http.Error(w, "backup id is required", http.StatusBadRequest)
		return
	}

	path, err := h.Manager.BackupDownloadPath(
		cellID,
		backupID,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set(
		"Content-Disposition",
		`attachment; filename="`+filepath.Base(path)+`"`,
	)

	http.ServeFile(w, r, path)
}

func (h *Handler) RestoreBackup(w http.ResponseWriter, r *http.Request) {
	cellID := r.PathValue("id")
	backupID := strings.TrimSpace(r.PathValue("backupID"))

	if invalidCellID(cellID) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureBackupsAllowed(w, cellID) {
		return
	}

	if backupID == "" {
		http.Error(w, "backup id is required", http.StatusBadRequest)
		return
	}

	if err := h.Manager.RestoreBackup(cellID, backupID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message":   "backup restored",
		"id":        cellID,
		"backup_id": backupID,
	})
}

func (h *Handler) ListBackupFiles(
	w http.ResponseWriter,
	r *http.Request,
) {
	cellID := r.PathValue("id")
	backupID := strings.TrimSpace(
		r.URL.Query().Get("backup_id"),
	)

	if invalidCellID(cellID) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureBackupsAllowed(w, cellID) {
		return
	}

	if backupID == "" {
		http.Error(w, "backup_id is required", http.StatusBadRequest)
		return
	}

	backupFiles, err := h.Manager.ListBackupFiles(
		cellID,
		backupID,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, backupFiles)
}

func (h *Handler) ReadBackupFile(
	w http.ResponseWriter,
	r *http.Request,
) {
	cellID := r.PathValue("id")
	backupID := strings.TrimSpace(
		r.URL.Query().Get("backup_id"),
	)
	path := strings.TrimSpace(
		r.URL.Query().Get("path"),
	)

	if invalidCellID(cellID) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureBackupsAllowed(w, cellID) {
		return
	}

	if backupID == "" {
		http.Error(w, "backup_id is required", http.StatusBadRequest)
		return
	}

	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	content, err := h.Manager.ReadBackupFile(
		cellID,
		backupID,
		path,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"backup_id": backupID,
		"path":      path,
		"content":   content,
	})
}

func (h *Handler) ExtractBackupFile(
	w http.ResponseWriter,
	r *http.Request,
) {
	cellID := r.PathValue("id")
	backupID := strings.TrimSpace(
		r.URL.Query().Get("backup_id"),
	)

	if invalidCellID(cellID) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureBackupsAllowed(w, cellID) {
		return
	}

	if backupID == "" {
		http.Error(w, "backup_id is required", http.StatusBadRequest)
		return
	}

	var request BackupExtractRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	request.Path = strings.TrimSpace(request.Path)

	if request.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	if err := h.Manager.ExtractBackupPath(
		cellID,
		backupID,
		request.Path,
	); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message":   "backup path extracted",
		"backup_id": backupID,
		"path":      request.Path,
	})
}

func (h *Handler) ListConfigFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	candidates := []map[string]string{
		{"id": "server.properties", "title": "Server Properties", "path": "server.properties", "description": "Main Minecraft server settings."},
		{"id": "spigot.yml", "title": "Spigot Configuration", "path": "spigot.yml", "description": "Spigot performance and gameplay settings."},
		{"id": "bukkit.yml", "title": "Bukkit Configuration", "path": "bukkit.yml", "description": "Bukkit plugin and world settings."},
		{"id": "paper-global.yml", "title": "Paper Global Settings", "path": "config/paper-global.yml", "description": "Global Paper server settings."},
		{"id": "paper-world-defaults.yml", "title": "Paper World Defaults", "path": "config/paper-world-defaults.yml", "description": "Default Paper world settings."},
		{"id": "purpur.yml", "title": "Purpur Configuration", "path": "purpur.yml", "description": "Purpur-specific server settings."},
	}

	available := make([]map[string]string, 0)

	for _, item := range candidates {
		if _, err := files.DownloadPath(gameServer.Dir, item["path"]); err == nil {
			available = append(available, item)
		}
	}

	writeJSON(w, map[string]any{
		"id":    id,
		"files": available,
	})
}

func (h *Handler) ReadConfigFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	content, err := files.Read(gameServer.Dir, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"id":      id,
		"path":    path,
		"content": content,
	})
}

func (h *Handler) WriteConfigFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureSettingsAllowed(w, id) {
		return
	}

	var request ConfigWriteRequest

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if request.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	if err := files.Write(gameServer.Dir, request.Path, request.Content); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "config file written",
		"path":    request.Path,
	})
}

func parseRconList(output string) []map[string]any {
	parts := strings.Split(output, ":")
	if len(parts) < 2 {
		return []map[string]any{}
	}

	namesPart := strings.TrimSpace(parts[len(parts)-1])
	if namesPart == "" {
		return []map[string]any{}
	}

	names := strings.Split(namesPart, ",")
	players := make([]map[string]any, 0)

	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		players = append(players, map[string]any{
			"name":   name,
			"online": true,
		})
	}

	return players
}

func (h *Handler) ListPlayers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	onlinePlayers, err := players.ListFromLogs(gameServer.Dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"id":      id,
		"source":  "logs",
		"players": onlinePlayers,
	})
}

func (h *Handler) KickPlayer(w http.ResponseWriter, r *http.Request) {
	h.playerCommand(w, r, "kick")
}

func (h *Handler) BanPlayer(w http.ResponseWriter, r *http.Request) {
	h.playerCommand(w, r, "ban")
}

func (h *Handler) OpPlayer(w http.ResponseWriter, r *http.Request) {
	h.playerCommand(w, r, "op")
}

func (h *Handler) DeopPlayer(w http.ResponseWriter, r *http.Request) {
	h.playerCommand(w, r, "deop")
}

func (h *Handler) playerCommand(w http.ResponseWriter, r *http.Request, action string) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureConsoleAllowed(w, id) {
		return
	}

	var request PlayerActionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if request.Name == "" {
		http.Error(w, "player name is required", http.StatusBadRequest)
		return
	}

	command := ""

	switch action {
	case "kick":
		if request.Reason != "" {
			command = "kick " + request.Name + " " + request.Reason
		} else {
			command = "kick " + request.Name
		}
	case "ban":
		if request.Reason != "" {
			command = "ban " + request.Name + " " + request.Reason
		} else {
			command = "ban " + request.Name
		}
	case "op":
		command = "op " + request.Name
	case "deop":
		command = "deop " + request.Name
	default:
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}

	if err := h.Manager.SendCommand(id, command); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "player command sent",
		"command": command,
	})
}

func (h *Handler) TestImporter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	var request ImporterRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	protocol := strings.ToLower(strings.TrimSpace(request.Protocol))

	switch protocol {
	case "local":
		if err := importer.TestLocal(importer.LocalConfig{
			SourcePath: request.RemotePath,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

	case "sftp":
		if err := importer.TestSFTP(importer.SFTPConfig{
			Host:                 request.Host,
			Port:                 request.Port,
			Username:             request.Username,
			AuthType:             request.AuthType,
			Password:             request.Password,
			PrivateKey:           request.PrivateKey,
			PrivateKeyPassphrase: request.PrivateKeyPassphrase,
			RemotePath:           request.RemotePath,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

	default:
		http.Error(
			w,
			"only sftp and local import protocols are supported",
			http.StatusBadRequest,
		)
		return
	}

	writeJSON(w, map[string]any{
		"message":  "source access successful",
		"id":       id,
		"protocol": protocol,
	})
}

func setImportProgress(id string, progress ImportProgress) {
	importProgressStore.Lock()
	importProgressStore.items[id] = progress
	importProgressStore.Unlock()
}

func getImportProgress(id string) ImportProgress {
	importProgressStore.RLock()
	progress, exists := importProgressStore.items[id]
	importProgressStore.RUnlock()

	if exists {
		return progress
	}

	return ImportProgress{
		Running: false,
		Stage:   "Waiting",
		Percent: 0,
		Message: "No active import job.",
	}
}

func setImportProgressForCell(gameCell *cell.Cell, progress ImportProgress) {
	setImportProgress(gameCell.ID, progress)
	_ = saveImportProgress(gameCell.Dir, progress)
}

func getImportProgressForCell(gameCell *cell.Cell) ImportProgress {
	progress := getImportProgress(gameCell.ID)

	if progress.Stage != "Waiting" || progress.Percent != 0 || progress.Running {
		return progress
	}

	diskProgress, err := loadImportProgress(gameCell.Dir)
	if err == nil {
		setImportProgress(gameCell.ID, diskProgress)
		return diskProgress
	}

	return progress
}

func saveImportProgress(cellDir string, progress ImportProgress) error {
	dir := filepath.Join(cellDir, ".hivepanel")

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(progress, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "import-progress.json"), data, 0644)
}

func loadImportProgress(cellDir string) (ImportProgress, error) {
	data, err := os.ReadFile(filepath.Join(cellDir, ".hivepanel", "import-progress.json"))
	if err != nil {
		return ImportProgress{}, err
	}

	var progress ImportProgress

	if err := json.Unmarshal(data, &progress); err != nil {
		return ImportProgress{}, err
	}

	return progress, nil
}

func (h *Handler) StartImporter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	if !h.ensureImporterAllowed(w, id) {
		return
	}

	var request ImporterRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	gameServer, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	protocol := strings.ToLower(strings.TrimSpace(request.Protocol))

	if protocol != "sftp" && protocol != "local" {
		http.Error(
			w,
			"only sftp and local import protocols are supported",
			http.StatusBadRequest,
		)
		return
	}

	if protocol == "local" {
		if err := importer.TestLocal(importer.LocalConfig{
			SourcePath: request.RemotePath,
			LocalPath:  gameServer.Dir,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	wasRunning := gameServer.Status == "running"

	if err := h.Manager.LockCell(
		id,
		"importing",
		"Importing Server",
		"This server is locked while files are imported from the migration source.",
	); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	setImportProgressForCell(gameServer, ImportProgress{
		Running: true,
		Stage:   "Queued",
		Percent: 5,
		Message: "Import job queued.",
	})

	go func() {
		defer h.Manager.UnlockCell(id)

		if wasRunning {
			setImportProgressForCell(gameServer, ImportProgress{
				Running: true,
				Stage:   "Stopping",
				Percent: 8,
				Message: "Stopping server before import...",
			})

			_ = h.Manager.Stop(id)

			if err := h.waitForCellStopped(id, 60*time.Second); err != nil {
				setImportProgressForCell(gameServer, ImportProgress{
					Running: false,
					Stage:   "Failed",
					Percent: 100,
					Message: "Failed to stop server before import.",
					Error:   err.Error(),
				})
				return
			}
		}

		options := importer.Options{
			ImportWorlds:     request.Options["importWorlds"] == true,
			ImportPlugins:    request.Options["importPlugins"] == true,
			ImportConfigs:    request.Options["importConfigs"] == true,
			ImportMods:       request.Options["importMods"] == true,
			ImportServerJar:  request.Options["importServerJar"] == true,
			WipeBeforeImport: request.Options["wipeBeforeImport"] == true,
		}

		progress := func(stage string, percent int, message string) {
			setImportProgressForCell(gameServer, ImportProgress{
				Running: true,
				Stage:   stage,
				Percent: percent,
				Message: message,
			})
		}

		var err error

		if protocol == "local" {
			err = importer.ImportLocal(importer.LocalConfig{
				SourcePath: request.RemotePath,
				LocalPath:  gameServer.Dir,
				Options:    options,
			}, progress)
		} else {
			err = importer.ImportSFTP(importer.SFTPConfig{
				Host:                 request.Host,
				Port:                 request.Port,
				Username:             request.Username,
				AuthType:             request.AuthType,
				Password:             request.Password,
				PrivateKey:           request.PrivateKey,
				PrivateKeyPassphrase: request.PrivateKeyPassphrase,
				RemotePath:           request.RemotePath,
				LocalPath:            gameServer.Dir,
				Options:              options,
			}, progress)
		}

		if err != nil {
			setImportProgressForCell(gameServer, ImportProgress{
				Running: false,
				Stage:   "Failed",
				Percent: 100,
				Message: "Import failed.",
				Error:   err.Error(),
			})
			return
		}

		if wasRunning {
			setImportProgressForCell(gameServer, ImportProgress{
				Running: true,
				Stage:   "Starting",
				Percent: 95,
				Message: "Restarting server...",
			})

			if err := h.Manager.Start(id); err != nil {
				setImportProgressForCell(gameServer, ImportProgress{
					Running: false,
					Stage:   "Complete",
					Percent: 100,
					Message: "Import completed, but the server could not be restarted.",
					Error:   err.Error(),
				})
				return
			}
		}

		setImportProgressForCell(gameServer, ImportProgress{
			Running: false,
			Stage:   "Complete",
			Percent: 100,
			Message: "Import completed successfully.",
		})
	}()

	writeJSON(w, map[string]any{
		"message":  "import job started",
		"id":       id,
		"status":   "running",
		"protocol": protocol,
	})
}

func (h *Handler) ImporterStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if invalidCellID(id) {
		http.Error(w, "invalid cell id", http.StatusBadRequest)
		return
	}

	gameCell, exists := h.Manager.Get(id)
	if !exists {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}

	progress := getImportProgressForCell(gameCell)

	writeJSON(w, map[string]any{
		"id":      id,
		"running": progress.Running,
		"stage":   progress.Stage,
		"percent": progress.Percent,
		"message": progress.Message,
		"error":   progress.Error,
	})
}

func (h *Handler) ensureCellUnlocked(w http.ResponseWriter, id string) bool {
	if err := h.Manager.EnsureNotLocked(id); err != nil {
		http.Error(w, err.Error(), http.StatusLocked)
		return false
	}

	return true
}

func (h *Handler) waitForCellStopped(id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		gameCell, err := h.Manager.Status(id)
		if err != nil {
			return err
		}

		if gameCell.Status == "offline" {
			return nil
		}

		time.Sleep(1 * time.Second)
	}

	return errors.New("timed out waiting for server to stop")
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func writeJSONStatus(
	w http.ResponseWriter,
	status int,
	data any,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func toFloat(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	default:
		return 0
	}
}

func roundFloat(value float64) float64 {
	return float64(int(value*100)) / 100
}

// DEBUG
func (h *Handler) LockCell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var request LockCellRequest
	_ = json.NewDecoder(r.Body).Decode(&request)

	if request.Type == "" {
		request.Type = "admin_lock"
	}
	if request.Reason == "" {
		request.Reason = "Server Locked"
	}
	if request.Message == "" {
		request.Message = "This server is temporarily locked."
	}

	if err := h.Manager.LockCell(id, request.Type, request.Reason, request.Message); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "cell locked",
		"id":      id,
	})
}

func (h *Handler) UnlockCell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := h.Manager.UnlockCell(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"message": "cell unlocked",
		"id":      id,
	})
}
