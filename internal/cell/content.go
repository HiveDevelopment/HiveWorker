package cell

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hivepanel-worker/internal/identity"
	"hivepanel-worker/internal/minecraft"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ContentJob struct {
	ID      string            `json:"id"`
	State   string            `json:"state"`
	Message string            `json:"message"`
	Logs    []string          `json:"logs"`
	Plan    minecraft.Plan    `json:"plan"`
	Result  *minecraft.Result `json:"result,omitempty"`
}

func (m *Manager) contentPath(id string) string {
	return filepath.Join(m.dataDir, "content-jobs", id+".json")
}
func (m *Manager) saveContentJob(id string, job *ContentJob) error {
	path := m.contentPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".job-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func (m *Manager) loadContentJob(id string) {
	data, err := os.ReadFile(m.contentPath(id))
	if err != nil {
		return
	}
	var job ContentJob
	if json.Unmarshal(data, &job) != nil {
		return
	}
	m.contentJobs[id] = &job
}
func (m *Manager) ContentStatus(id string) (*ContentJob, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	if _, ok := m.cells[id]; !ok {
		return nil, errors.New("cell not found")
	}
	job := m.contentJobs[id]
	if job == nil {
		return &ContentJob{State: "idle", Logs: []string{}}, nil
	}
	copy := *job
	copy.Logs = append([]string{}, job.Logs...)
	return &copy, nil
}
func (m *Manager) StartContentInstall(id, registry string, plan minecraft.Plan) (*ContentJob, error) {
	if err := minecraft.Validate(plan, registry); err != nil {
		return nil, err
	}
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.mutex.Lock()
	defer m.mutex.Unlock()
	c, ok := m.cells[id]
	if !ok {
		return nil, errors.New("cell not found")
	}
	if c.Lock.Locked || c.InstallStatus != "installed" || m.runtime.IsRunning(id) {
		return nil, errors.New("Cell must be installed, unlocked and stopped before installing content")
	}
	_, uid, gid, err := identity.Ensure()
	if err != nil {
		return nil, err
	}
	image := c.Docker.Image
	if image == "" {
		comb, err := m.resolveCellComb(c)
		if err != nil {
			return nil, err
		}
		image = comb.Image
	}
	bytes := make([]byte, 16)
	if _, err = rand.Read(bytes); err != nil {
		return nil, err
	}
	job := &ContentJob{ID: hex.EncodeToString(bytes), State: "installing", Message: "Preparing installation", Logs: []string{}, Plan: plan}
	c.Lock = CellLock{Locked: true, Type: "content_install", Message: "Installing Minecraft content", DisablePower: true, DisableFiles: true, DisableBackups: true, DisableSettings: true, DisableImporter: true}
	if err = saveLock(c.Dir, c.Lock); err != nil {
		c.Lock = CellLock{}
		return nil, err
	}
	if err = m.save(c); err != nil {
		c.Lock = CellLock{}
		_ = saveLock(c.Dir, c.Lock)
		return nil, err
	}
	m.contentJobs[id] = job
	if err = m.saveContentJob(id, job); err != nil {
		c.Lock = CellLock{}
		_ = saveLock(c.Dir, c.Lock)
		_ = m.save(c)
		delete(m.contentJobs, id)
		return nil, err
	}
	dir := c.Dir
	go m.runContentInstall(id, dir, image, registry, plan, uid, gid)
	copy := *job
	return &copy, nil
}
func (m *Manager) runContentInstall(id, dir, image, registry string, plan minecraft.Plan, uid, gid int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	logger := func(line string) {
		m.mutex.Lock()
		job := m.contentJobs[id]
		job.Logs = append(job.Logs, line)
		if len(job.Logs) > 500 {
			job.Logs = job.Logs[len(job.Logs)-500:]
		}
		job.Message = line
		_ = m.saveContentJob(id, job)
		m.mutex.Unlock()
		m.broadcastByID(id, line)
	}
	result, err := minecraft.Install(ctx, dir, image, registry, plan, logger, identity.Prepare, uid, gid, func(result minecraft.Result) error {
		m.mutex.Lock()
		defer m.mutex.Unlock()
		job := m.contentJobs[id]
		job.Result = &result
		return m.saveContentJob(id, job)
	})
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.mutex.Lock()
	defer m.mutex.Unlock()
	c := m.cells[id]
	job := m.contentJobs[id]
	if c == nil {
		return
	}
	if err != nil {
		job.State = "failed"
		job.Message = err.Error()
	} else {
		job.Result = &result
		applyContentResult(c, job)
		job.State = "complete"
		job.Message = fmt.Sprintf("Installed %d files. Replaced files retained in %s", result.Files, result.Backup)
	}
	// Never release the Cell if an incomplete file transaction could not be recovered.
	if recoveryErr := minecraft.Recover(dir); recoveryErr != nil {
		job.State = "failed"
		job.Message += "; recovery failed: " + recoveryErr.Error()
	} else {
		c.Lock = CellLock{}
		_ = saveLock(dir, c.Lock)
	}
	if saveErr := m.save(c); saveErr != nil {
		c.Lock = CellLock{Locked: true, Type: "content_install", Message: "Content installed but definition could not be saved", DisablePower: true, DisableFiles: true, DisableSettings: true}
		_ = saveLock(dir, c.Lock)
		job.State = "failed"
		job.Message += "; saving Cell definition failed: " + saveErr.Error()
	}
	_ = m.saveContentJob(id, job)
}

func applyContentResult(c *Cell, job *ContentJob) {
	if job.Result == nil || job.Result.Startup == "" {
		return
	}
	c.Startup.Command = job.Result.Startup
	variables := make(map[string]string, len(c.Variables)+4)
	for key, value := range c.Variables {
		variables[key] = value
	}
	variables["version"] = job.Plan.Minecraft
	variables["loader_version"] = job.Plan.LoaderVersion
	if job.Plan.Loader == "forge" {
		variables["forge_version"] = strings.TrimPrefix(job.Plan.LoaderVersion, job.Plan.Minecraft+"-")
	}
	if job.Plan.Loader == "fabric" {
		variables["fabric_version"] = job.Plan.LoaderVersion
	}
	if job.Plan.Loader == "neoforge" {
		variables["neoforge_version"] = job.Plan.LoaderVersion
	}
	c.Variables = variables
}

func (m *Manager) recoverContent(c *Cell) {
	job := m.contentJobs[c.ID]
	committed := job != nil && job.Result != nil && minecraft.TransactionCommitted(job.Result.Backup)
	if err := minecraft.Recover(c.Dir); err != nil {
		c.Lock.Message = "Content recovery failed: " + err.Error()
		if job != nil {
			job.State = "failed"
			job.Message = c.Lock.Message
			_ = m.saveContentJob(c.ID, job)
		}
		_ = saveLock(c.Dir, c.Lock)
		return
	}
	if committed {
		applyContentResult(c, job)
		job.State = "complete"
		job.Message = "Installation committed before Worker restart; startup restored."
	} else if job != nil && job.State == "installing" {
		job.State = "failed"
		job.Message = "Installation interrupted by a Worker restart. Previous files were restored."
	}
	c.Lock = CellLock{}
	err := m.save(c)
	if err == nil {
		err = saveLock(c.Dir, c.Lock)
	}
	if err != nil {
		c.Lock = CellLock{Locked: true, Type: "content_install", Message: "Content recovery could not save the Cell definition: " + err.Error(), DisablePower: true, DisableFiles: true, DisableSettings: true}
		_ = saveLock(c.Dir, c.Lock)
		if job != nil {
			job.State = "failed"
			job.Message = c.Lock.Message
		}
	}
	if job != nil {
		_ = m.saveContentJob(c.ID, job)
	}
}
