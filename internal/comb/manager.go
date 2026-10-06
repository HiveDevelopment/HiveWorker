package comb

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Comb struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Category     string            `json:"category,omitempty"`
	Group        string            `json:"group,omitempty"`
	Game         string            `json:"game,omitempty"`
	Tags         []string          `json:"tags,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Startup      string            `json:"startup"`
	Variables    map[string]string `json:"variables"`
	Install      []InstallStep     `json:"install"`
	Image        string            `json:"image"`
	WorkingDir   string            `json:"working_dir"`
	Entrypoint   []string          `json:"entrypoint"`
	Environment  map[string]string `json:"environment"`
}

type InstallStep struct {
	ID   string         `json:"id,omitempty"`
	Type string         `json:"type"`
	With map[string]any `json:"with,omitempty"`
	Save string         `json:"save,omitempty"`
}

type Manager struct {
	combs map[string]*Comb
	dir   string
}

func NewManager(dataDir string) *Manager {
	return &Manager{
		combs: map[string]*Comb{},
		dir:   filepath.Join(dataDir, "combs"),
	}
}

func (m *Manager) Load() error {
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		return err
	}

	m.combs = map[string]*Comb{}

	return filepath.WalkDir(m.dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		var loaded Comb
		if err := json.Unmarshal(data, &loaded); err != nil {
			return err
		}

		if loaded.ID == "" {
			return errors.New("comb id is required: " + path)
		}

		m.combs[loaded.ID] = &loaded

		return nil
	})
}

func (m *Manager) List() []*Comb {
	list := make([]*Comb, 0, len(m.combs))

	for _, comb := range m.combs {
		list = append(list, comb)
	}

	return list
}

func (m *Manager) Get(id string) (*Comb, bool) {
	comb, exists := m.combs[id]
	return comb, exists
}

func (m *Manager) Require(id string) (*Comb, error) {
	comb, exists := m.Get(id)
	if !exists {
		return nil, errors.New("comb not found")
	}

	return comb, nil
}

func FromMap(data map[string]any) (*Comb, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	var c Comb
	if err := json.Unmarshal(bytes, &c); err != nil {
		return nil, err
	}

	if c.ID == "" {
		return nil, errors.New("comb id is required")
	}

	return &c, nil
}
