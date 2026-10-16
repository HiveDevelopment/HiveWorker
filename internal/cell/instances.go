package cell

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"hivepanel-worker/internal/filelease"
	"hivepanel-worker/internal/identity"
	"hivepanel-worker/internal/instances"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type InstanceDefinition struct {
	Comb          string            `json:"comb"`
	CombData      map[string]any    `json:"comb_data"`
	Variables     map[string]string `json:"variables"`
	Docker        DockerConfig      `json:"docker"`
	Startup       StartupConfig     `json:"startup"`
	InstallStatus string            `json:"install_status"`
	InstallError  string            `json:"install_error"`
	ContentJob    *ContentJob       `json:"content_job,omitempty"`
}
type SavedInstance struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	CreatedAt  string             `json:"created_at"`
	Definition InstanceDefinition `json:"definition"`
}
type InstanceState struct {
	Active  string          `json:"active"`
	Entries []SavedInstance `json:"entries"`
}
type instanceJournal struct {
	From  string        `json:"from"`
	To    string        `json:"to"`
	State InstanceState `json:"state"`
	Phase string        `json:"phase"`
	Token string        `json:"token,omitempty"`
}

func (m *Manager) instanceRoot(id string) string {
	return filepath.Join(m.instancesDir, ".hive-instances", id)
}
func (m *Manager) instanceStatePath(id string) string {
	return filepath.Join(m.dataDir, "instance-profiles", id+".json")
}
func (m *Manager) instanceJournalPath(id string) string {
	return filepath.Join(m.dataDir, "instance-profiles", id+".journal.json")
}
func writeInstanceJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".instance-")
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
func snapshotDefinition(c *Cell, job *ContentJob) InstanceDefinition {
	return InstanceDefinition{Comb: c.Comb, CombData: c.CombData, Variables: c.Variables, Docker: c.Docker, Startup: c.Startup, InstallStatus: c.InstallStatus, InstallError: c.InstallError, ContentJob: job}
}
func applyInstanceDefinition(c *Cell, d InstanceDefinition) {
	c.Comb = d.Comb
	c.CombData = d.CombData
	c.Variables = map[string]string{}
	for key, value := range d.Variables {
		c.Variables[key] = value
	}
	c.Variables["memory"] = strconv.Itoa(c.Limits.MemoryMB)
	c.Variables["server_port"] = strconv.Itoa(c.Allocation.Port)
	c.Variables["server_ip"] = c.Allocation.IP
	c.Docker = d.Docker
	c.Startup = d.Startup
	c.InstallStatus = d.InstallStatus
	c.InstallError = d.InstallError
	c.ConfigurationPending = false
	c.ActiveAllocations = nil
	c.ActiveStop = nil
}
func (m *Manager) loadInstanceState(c *Cell) (InstanceState, error) {
	var state InstanceState
	data, err := os.ReadFile(m.instanceStatePath(c.ID))
	if errors.Is(err, os.ErrNotExist) {
		return InstanceState{Active: "default", Entries: []SavedInstance{{ID: "default", Name: "Default", CreatedAt: time.Now().UTC().Format(time.RFC3339), Definition: snapshotDefinition(c, m.contentJobs[c.ID])}}}, nil
	}
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	found := false
	for _, entry := range state.Entries {
		if !validInstanceID(entry.ID) {
			return state, errors.New("invalid saved instance identifier")
		}
		if entry.ID == state.Active {
			found = true
		}
	}
	if !found {
		return state, errors.New("active instance is missing from its profile")
	}
	return state, nil
}
func validInstanceID(id string) bool {
	if id == "default" {
		return true
	}
	_, err := uuid.Parse(id)
	return err == nil
}
func (m *Manager) instanceReady(id string) (*Cell, error) {
	c, ok := m.cells[id]
	if !ok {
		return nil, errors.New("Cell not found")
	}
	if c.Lock.Locked || c.InstallStatus == "installing" || c.InstallStatus == "pending" || m.runtime.IsRunning(id) {
		return nil, errors.New("stop the Cell and wait for current operations before managing instances")
	}
	if _, err := os.Stat(m.instanceJournalPath(id)); err == nil {
		return nil, errors.New("an instance activation needs recovery before continuing")
	}
	return c, nil
}
func (m *Manager) InstanceList(id string) (map[string]any, error) {
	m.mutex.RLock()
	c, ok := m.cells[id]
	if !ok {
		m.mutex.RUnlock()
		return nil, errors.New("Cell not found")
	}
	state, err := m.loadInstanceState(c)
	activeDir := c.Dir
	limit := instanceLimit(c)
	disk := c.Limits.DiskMB
	activeComb := c.Comb
	activeImage := c.Docker.Image
	m.mutex.RUnlock()
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, entry := range state.Entries {
		root := filepath.Join(m.instanceRoot(id), entry.ID)
		comb, image := entry.Definition.Comb, entry.Definition.Docker.Image
		if entry.ID == state.Active {
			root = activeDir
			comb = activeComb
			image = activeImage
		}
		size, err := instances.Size(root)
		if err != nil {
			return nil, err
		}
		rows = append(rows, map[string]any{"id": entry.ID, "name": entry.Name, "created_at": entry.CreatedAt, "active": entry.ID == state.Active, "comb": comb, "image": image, "bytes": size})
	}
	var job InstanceCopyJob
	if bytes, err := os.ReadFile(m.copyJobPath(id)); err == nil {
		_ = json.Unmarshal(bytes, &job)
	}
	return map[string]any{"active": state.Active, "instances": rows, "limit": limit, "disk_limit_mb": disk, "operation": job}, nil
}
func instanceLimit(c *Cell) int {
	if c.FeatureLimits.InstanceLimit != nil {
		return max(0, min(20, *c.FeatureLimits.InstanceLimit))
	}
	return 5
}

type InstanceCopyJob struct {
	ID     string `json:"id"`
	Target string `json:"target"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Error  string `json:"error"`
}

func (m *Manager) copyJobPath(id string) string {
	return filepath.Join(m.dataDir, "instance-profiles", id+".copy.json")
}
func (m *Manager) beginInstanceCopy(id, name string, empty bool) (InstanceCopyJob, <-chan error, error) {
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.mutex.Lock()
	defer m.mutex.Unlock()
	c, err := m.instanceReady(id)
	if err != nil {
		return InstanceCopyJob{}, nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		return InstanceCopyJob{}, nil, errors.New("choose an instance name up to 80 characters")
	}
	state, err := m.loadInstanceState(c)
	if err != nil {
		return InstanceCopyJob{}, nil, err
	}
	if len(state.Entries) >= instanceLimit(c) {
		return InstanceCopyJob{}, nil, errors.New("instance limit reached")
	}
	for i := range state.Entries {
		if strings.EqualFold(state.Entries[i].Name, name) {
			return InstanceCopyJob{}, nil, errors.New("instance name already exists")
		}
		if state.Entries[i].ID == state.Active {
			state.Entries[i].Definition = snapshotDefinition(c, m.contentJobs[id])
		}
	}
	entry := SavedInstance{ID: uuid.NewString(), Name: name, CreatedAt: time.Now().UTC().Format(time.RFC3339), Definition: snapshotDefinition(c, m.contentJobs[id])}
	if empty {
		entry.Definition.InstallStatus = "pending"
		entry.Definition.InstallError = ""
		entry.Definition.ContentJob = nil
	}
	job := InstanceCopyJob{ID: uuid.NewString(), Target: entry.ID, Name: name, State: "copying"}
	if err = writeInstanceJSON(m.copyJobPath(id), job); err != nil {
		return InstanceCopyJob{}, nil, err
	}
	c.Lock = DefaultLock("instance_operation", "Saving instance", "An instance is being saved. File and power controls will return when it finishes.")
	c.Lock.OperationToken = job.ID
	if err = saveLock(c.Dir, c.Lock); err != nil {
		c.Lock = CellLock{}
		job.State = "failed"
		job.Error = "Could not lock the Cell for an instance save"
		_ = writeInstanceJSON(m.copyJobPath(id), job)
		return InstanceCopyJob{}, nil, err
	}
	copy := *c
	done := make(chan error, 1)
	go func() {
		err := m.copyInstanceFiles(&copy, state, entry, empty)
		m.definitionMutex.Lock()
		defer m.definitionMutex.Unlock()
		m.mutex.Lock()
		defer m.mutex.Unlock()
		current, exists := m.cells[id]
		if !exists {
			done <- errors.New("Cell was removed")
			return
		}
		if err == nil {
			job.State = "complete"
		} else {
			job.State = "failed"
			job.Error = "Instance save failed. Check the shared disk allowance and Worker journal."
			log.Printf("Instance save failed for Cell %s: %v", id, err)
		}
		_ = writeInstanceJSON(m.copyJobPath(id), job)
		if current.Lock.Type == "instance_operation" && current.Lock.OperationToken == job.ID {
			current.Lock = CellLock{}
			_ = deleteLock(current.Dir)
		}
		delete(m.diskCache, id)
		done <- err
	}()
	return job, done, nil
}
func (m *Manager) CreateInstance(id, name string, empty bool) (SavedInstance, error) {
	job, done, err := m.beginInstanceCopy(id, name, empty)
	if err != nil {
		return SavedInstance{}, err
	}
	if err = <-done; err != nil {
		return SavedInstance{}, err
	}
	return SavedInstance{ID: job.Target, Name: job.Name}, nil
}
func (m *Manager) StartCreateInstance(id, name string, empty bool) (InstanceCopyJob, error) {
	job, _, err := m.beginInstanceCopy(id, name, empty)
	return job, err
}
func (m *Manager) copyInstanceFiles(c *Cell, state InstanceState, entry SavedInstance, empty bool) error {
	release := filelease.Write(c.Dir)
	defer release()
	root := m.instanceRoot(c.ID)
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	activeBytes, err := instances.Size(c.Dir)
	if err != nil {
		return err
	}
	savedBytes, err := instances.Size(root)
	if err != nil {
		return err
	}
	budget := int64(-1)
	if c.Limits.DiskMB > 0 {
		budget = int64(c.Limits.DiskMB)*1024*1024 - activeBytes - savedBytes
		if budget < 0 || (!empty && activeBytes > budget) {
			return errors.New("saved instance would exceed the shared Cell disk allowance")
		}
	}
	target := filepath.Join(root, entry.ID)
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(target)
		}
	}()
	if empty {
		err = os.Mkdir(target, 0700)
	} else {
		err = instances.Copy(c.Dir, target, budget)
	}
	if err != nil {
		return err
	}
	_ = deleteLock(target)
	prepare := m.prepareInstance
	if prepare == nil {
		prepare = identity.Prepare
	}
	if err = prepare(target); err != nil {
		return err
	}
	state.Entries = append(state.Entries, entry)
	if err = writeInstanceJSON(m.instanceStatePath(c.ID), state); err != nil {
		return err
	}
	complete = true
	return nil
}
func (m *Manager) recoverInstanceCopy(c *Cell) error {
	data, err := os.ReadFile(m.copyJobPath(c.ID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var job InstanceCopyJob
	if json.Unmarshal(data, &job) != nil || !validInstanceID(job.Target) {
		return errors.New("invalid instance copy state")
	}
	if job.State != "copying" {
		return nil
	}
	state, err := m.loadInstanceState(c)
	if err != nil {
		return err
	}
	committed := false
	for _, entry := range state.Entries {
		if entry.ID == job.Target {
			committed = true
		}
	}
	if committed {
		job.State = "complete"
	} else {
		_ = os.RemoveAll(filepath.Join(m.instanceRoot(c.ID), job.Target))
		job.State = "failed"
		job.Error = "Instance save was interrupted by a Worker restart. Your active files were kept."
	}
	if c.Lock.Type == "instance_operation" && c.Lock.OperationToken == job.ID {
		c.Lock = CellLock{}
		_ = deleteLock(c.Dir)
	}
	return writeInstanceJSON(m.copyJobPath(c.ID), job)
}
func (m *Manager) ActivateInstance(id, target string, tokens ...string) (*Cell, error) {
	copy, ok := m.Get(id)
	if !ok {
		return nil, errors.New("Cell not found")
	}
	release := filelease.Write(copy.Dir)
	defer release()
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.mutex.Lock()
	defer m.mutex.Unlock()
	token := ""
	if len(tokens) > 0 {
		token = tokens[0]
		if token != "" {
			if _, err := uuid.Parse(token); err != nil {
				return nil, errors.New("invalid activation token")
			}
		}
	}
	// The Panel may retry after a lost response or database failure.
	if current := m.cells[id]; current != nil && token != "" && current.Lock.Type == "resource_operation" && current.Lock.OperationToken == token {
		state, err := m.loadInstanceState(current)
		if err == nil && state.Active == target {
			return current, nil
		}
	}
	c, err := m.instanceReady(id)
	if err != nil {
		return nil, err
	}
	state, err := m.loadInstanceState(c)
	if err != nil {
		return nil, err
	}
	if target == state.Active {
		if token != "" {
			c.Lock = DefaultLock("resource_operation", "Synchronizing instance", "Retry activating this instance in the panel to finish synchronizing its settings.")
			c.Lock.OperationToken = token
			if err := saveLock(c.Dir, c.Lock); err != nil {
				return nil, err
			}
		}
		return c, nil
	}
	found := false
	var definition InstanceDefinition
	for i := range state.Entries {
		if state.Entries[i].ID == state.Active {
			state.Entries[i].Definition = snapshotDefinition(c, m.contentJobs[id])
		}
		if state.Entries[i].ID == target {
			found = true
			definition = state.Entries[i].Definition
		}
	}
	if !found || !validInstanceID(target) {
		return nil, errors.New("instance not found")
	}
	if disposable, ok := m.runtime.(interface{ Delete(string) error }); ok {
		if err := disposable.Delete(id); err != nil {
			return nil, err
		}
	}
	old := state.Active
	inactive := filepath.Join(m.instanceRoot(id), target)
	oldRoot := filepath.Join(m.instanceRoot(id), old)
	if _, err = os.Lstat(oldRoot); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("inactive directory for the active instance already exists")
	}
	info, err := os.Lstat(inactive)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("saved instance directory is unavailable")
	}
	journal := instanceJournal{From: old, To: target, State: state, Phase: "prepared", Token: token}
	if err = writeInstanceJSON(m.instanceJournalPath(id), journal); err != nil {
		return nil, err
	}
	guarded := true
	defer func() {
		if guarded {
			c.Lock = DefaultLock("instance_recovery", "Instance recovery needed", "Instance activation requires recovery before this Cell can start.")
			_ = saveLock(c.Dir, c.Lock)
		}
	}()
	c.Lock = DefaultLock("instance_recovery", "Activating instance", "Instance activation must finish before this Cell can start.")
	if err = saveLock(c.Dir, c.Lock); err != nil {
		return nil, err
	}
	if err = os.Rename(c.Dir, oldRoot); err != nil {
		_ = os.Remove(m.instanceJournalPath(id))
		c.Lock = CellLock{}
		_ = deleteLock(c.Dir)
		guarded = false
		return nil, err
	}
	if err = os.Rename(inactive, c.Dir); err != nil {
		if rollbackErr := os.Rename(oldRoot, c.Dir); rollbackErr == nil {
			_ = os.Remove(m.instanceJournalPath(id))
			c.Lock = CellLock{}
			_ = deleteLock(c.Dir)
			guarded = false
		}
		return nil, err
	}
	journal.Phase = "swapped"
	if err = writeInstanceJSON(m.instanceJournalPath(id), journal); err != nil {
		return nil, err
	}
	applyInstanceDefinition(c, definition)
	state.Active = target
	if err = m.save(c); err != nil {
		return nil, err
	}
	if definition.ContentJob != nil {
		m.contentJobs[id] = definition.ContentJob
		_ = m.saveContentJob(id, definition.ContentJob)
	} else {
		delete(m.contentJobs, id)
		_ = os.Remove(m.contentPath(id))
	}
	if err = writeInstanceJSON(m.instanceStatePath(id), state); err != nil {
		return nil, err
	}
	c.Lock = CellLock{}
	if token != "" {
		c.Lock = DefaultLock("resource_operation", "Synchronizing instance", "Retry activating this instance in the panel to finish synchronizing its settings.")
		c.Lock.OperationToken = token
		err = saveLock(c.Dir, c.Lock)
	} else {
		err = deleteLock(c.Dir)
	}
	if err != nil {
		return nil, err
	}
	if err = m.save(c); err != nil {
		return nil, err
	}
	if err = os.Remove(m.instanceJournalPath(id)); err != nil {
		return nil, err
	}
	guarded = false
	delete(m.diskCache, id)
	return c, nil
}
func (m *Manager) DeleteInstance(id, target string) error {
	m.definitionMutex.Lock()
	defer m.definitionMutex.Unlock()
	m.mutex.Lock()
	defer m.mutex.Unlock()
	c, err := m.instanceReady(id)
	if err != nil {
		return err
	}
	state, err := m.loadInstanceState(c)
	if err != nil {
		return err
	}
	if target == state.Active {
		return errors.New("activate another instance before deleting this one")
	}
	index := -1
	for i, e := range state.Entries {
		if e.ID == target {
			index = i
		}
	}
	if index < 0 || !validInstanceID(target) {
		return errors.New("instance not found")
	}
	// Rename to a tombstone before changing metadata; restore it if saving fails.
	source := filepath.Join(m.instanceRoot(id), target)
	tomb := source + ".deleting"
	if err = os.Rename(source, tomb); err != nil {
		return err
	}
	state.Entries = append(state.Entries[:index], state.Entries[index+1:]...)
	if err = writeInstanceJSON(m.instanceStatePath(id), state); err != nil {
		_ = os.Rename(tomb, source)
		return err
	}
	delete(m.diskCache, id)
	return os.RemoveAll(tomb)
}
func (m *Manager) recoverInstance(c *Cell) error {
	data, err := os.ReadFile(m.instanceJournalPath(c.ID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var j instanceJournal
	if json.Unmarshal(data, &j) != nil || !validInstanceID(j.From) || !validInstanceID(j.To) {
		return errors.New("invalid instance recovery journal")
	}
	oldRoot := filepath.Join(m.instanceRoot(c.ID), j.From)
	newRoot := filepath.Join(m.instanceRoot(c.ID), j.To)
	_, oldErr := os.Lstat(oldRoot)
	_, activeErr := os.Lstat(c.Dir)
	_, newErr := os.Lstat(newRoot)
	if oldErr == nil && activeErr != nil && newErr == nil {
		if err = os.Rename(oldRoot, c.Dir); err != nil {
			return err
		}
		c.Lock = CellLock{}
		_ = deleteLock(c.Dir)
		return os.Remove(m.instanceJournalPath(c.ID))
	}
	if oldErr == nil && activeErr == nil && errors.Is(newErr, os.ErrNotExist) {
		for _, e := range j.State.Entries {
			if e.ID == j.To {
				applyInstanceDefinition(c, e.Definition)
				c.Lock = CellLock{}
				_ = deleteLock(c.Dir)
				if e.Definition.ContentJob != nil {
					m.contentJobs[c.ID] = e.Definition.ContentJob
					_ = m.saveContentJob(c.ID, e.Definition.ContentJob)
				} else {
					delete(m.contentJobs, c.ID)
					_ = os.Remove(m.contentPath(c.ID))
				}
				j.State.Active = j.To
				if err = m.save(c); err != nil {
					return err
				}
				if err = writeInstanceJSON(m.instanceStatePath(c.ID), j.State); err != nil {
					return err
				}
				c.Lock = CellLock{}
				if j.Token != "" {
					c.Lock = DefaultLock("resource_operation", "Synchronizing instance", "Retry activating this instance in the panel to finish synchronizing its settings.")
					c.Lock.OperationToken = j.Token
					if err = saveLock(c.Dir, c.Lock); err != nil {
						return err
					}
				} else {
					_ = deleteLock(c.Dir)
				}
				return os.Remove(m.instanceJournalPath(c.ID))
			}
		}
	}
	if errors.Is(oldErr, os.ErrNotExist) && activeErr == nil && newErr == nil {
		c.Lock = CellLock{}
		_ = deleteLock(c.Dir)
		return os.Remove(m.instanceJournalPath(c.ID))
	}
	return fmt.Errorf("instance activation recovery requires administrator inspection for Cell %s", c.ID)
}
