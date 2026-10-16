// Package firewall owns only HivePanel's filtering chains; it never flushes Docker rules.
package firewall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hivepanel-worker/internal/allocation"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type Rule struct {
	Source string `json:"source"`
	Action string `json:"action"`
	Port   int    `json:"port"`
	Note   string `json:"note"`
}
type Policy struct {
	Enabled bool   `json:"enabled"`
	Default string `json:"default"`
	Rules   []Rule `json:"rules"`
}
type State struct {
	Policies    map[string]Policy                  `json:"policies"`
	Allocations map[string][]allocation.Allocation `json:"allocations"`
}
type Runner interface {
	Run(context.Context, string, []string, string) ([]byte, error)
}
type SystemRunner struct{}

func (SystemRunner) Run(ctx context.Context, command string, args []string, input string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = strings.NewReader(input)
	return cmd.CombinedOutput()
}

type Manager struct {
	mu        sync.Mutex
	path      string
	state     State
	runner    Runner
	snapshot  atomic.Value
	Hosts     func() ([]netip.Addr, error)
	OnPolicy  func(context.Context, string, Policy) error
	lastError string
}

func New(data string, runner Runner) (*Manager, error) {
	if runner == nil {
		runner = SystemRunner{}
	}
	m := &Manager{path: filepath.Join(data, "firewall.json"), runner: runner, Hosts: HostAddresses, state: State{Policies: map[string]Policy{}, Allocations: map[string][]allocation.Allocation{}}}
	bytes, err := os.ReadFile(m.path)
	if err == nil {
		if err = json.Unmarshal(bytes, &m.state); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if m.state.Policies == nil {
		m.state.Policies = map[string]Policy{}
	}
	if m.state.Allocations == nil {
		m.state.Allocations = map[string][]allocation.Allocation{}
	}
	for id, p := range m.state.Policies {
		p, err = Validate(p)
		if err != nil {
			return nil, fmt.Errorf("invalid persisted firewall policy for %s", id)
		}
		m.state.Policies[id] = p
	}
	m.publish()
	return m, nil
}
func Default() Policy { return Policy{Default: "allow", Rules: []Rule{}} }
func Validate(p Policy) (Policy, error) {
	if p.Default != "allow" && p.Default != "deny" {
		return p, errors.New("default action must be allow or deny")
	}
	if len(p.Rules) > 100 {
		return p, errors.New("maximum 100 firewall rules per Cell")
	}
	for i, r := range p.Rules {
		if r.Action != "allow" && r.Action != "deny" {
			return p, errors.New("rule action must be allow or deny")
		}
		if r.Port < 0 || r.Port > 65535 || len(r.Note) > 200 || strings.ContainsAny(r.Note, "\x00\r\n") {
			return p, errors.New("invalid rule port or note")
		}
		source := strings.TrimSpace(r.Source)
		if addr, err := netip.ParseAddr(source); err == nil {
			if addr.Is4In6() {
				return p, errors.New("use a normal IPv4 address rather than IPv4-mapped IPv6")
			}
			r.Source = netip.PrefixFrom(addr, addr.BitLen()).String()
		} else if prefix, err := netip.ParsePrefix(source); err == nil && !prefix.Addr().Is4In6() {
			r.Source = prefix.Masked().String()
		} else {
			return p, errors.New("enter an IPv4/IPv6 address or CIDR network")
		}
		p.Rules[i] = r
	}
	if p.Rules == nil {
		p.Rules = []Rule{}
	}
	return p, nil
}
func (m *Manager) publish() {
	snapshot := map[string]Policy{}
	for id, p := range m.state.Policies {
		p.Rules = append([]Rule{}, p.Rules...)
		snapshot[id] = p
	}
	m.snapshot.Store(snapshot)
}
func (m *Manager) Policy(id string) Policy {
	if p, ok := m.snapshot.Load().(map[string]Policy)[id]; ok {
		return p
	}
	return Default()
}
func (m *Manager) Status(id string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := Default()
	if current, ok := m.state.Policies[id]; ok {
		p = current
	}
	return map[string]any{"policy": p, "error": m.lastError, "backend": "iptables", "ports": m.state.Allocations[id]}
}
func (m *Manager) Set(ctx context.Context, id string, p Policy) error {
	p, err := Validate(p)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	allocs := m.state.Allocations[id]
	ports := map[int]bool{}
	for _, a := range allocs {
		ports[a.Port] = true
	}
	for _, r := range p.Rules {
		if r.Port != 0 && !ports[r.Port] {
			return errors.New("choose an allocation assigned to this Cell")
		}
	}
	old := m.state.Policies[id]
	_, exists := m.state.Policies[id]
	m.state.Policies[id] = p
	restore := func() {
		if exists {
			m.state.Policies[id] = old
		} else {
			delete(m.state.Policies, id)
		}
		m.publish()
	}
	// Publish to Nginx only after host rules apply; restore both on any failure.
	if err := m.applyLocked(ctx); err != nil {
		restore()
		_ = m.applyLocked(ctx)
		m.lastError = err.Error()
		return err
	}
	m.publish()
	if m.OnPolicy != nil {
		if err := m.OnPolicy(ctx, id, p); err != nil {
			restore()
			_ = m.applyLocked(ctx)
			return err
		}
	}
	if err := m.saveLocked(); err != nil {
		restore()
		_ = m.applyLocked(ctx)
		if m.OnPolicy != nil {
			_ = m.OnPolicy(ctx, id, m.Policy(id))
		}
		return err
	}
	m.lastError = ""
	return nil
}
func (m *Manager) Sync(ctx context.Context, id string, allocs []allocation.Allocation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.state.Allocations[id]
	m.state.Allocations[id] = append([]allocation.Allocation{}, allocs...)
	if m.Policy(id).Enabled {
		if err := m.applyLocked(ctx); err != nil {
			m.state.Allocations[id] = previous
			_ = m.applyLocked(ctx)
			m.lastError = err.Error()
			return err
		}
	}
	if err := m.saveLocked(); err != nil {
		m.state.Allocations[id] = previous
		if m.Policy(id).Enabled {
			_ = m.applyLocked(ctx)
		}
		return err
	}
	return nil
}
func (m *Manager) Remove(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, exists := m.state.Policies[id]
	allocs := m.state.Allocations[id]
	delete(m.state.Policies, id)
	delete(m.state.Allocations, id)
	if exists && p.Enabled {
		if err := m.applyLocked(ctx); err != nil {
			m.state.Policies[id] = p
			m.state.Allocations[id] = allocs
			return err
		}
	}
	if err := m.saveLocked(); err != nil {
		if exists {
			m.state.Policies[id] = p
		}
		m.state.Allocations[id] = allocs
		_ = m.applyLocked(ctx)
		m.publish()
		return err
	}
	m.publish()
	return nil
}
func (m *Manager) Restore(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasEnabled() {
		return nil
	}
	err := m.applyLocked(ctx)
	if err != nil {
		m.lastError = err.Error()
	} else {
		m.lastError = ""
	}
	return err
}
func (m *Manager) hasEnabled() bool {
	for _, p := range m.state.Policies {
		if p.Enabled {
			return true
		}
	}
	return false
}
func (m *Manager) saveLocked() error {
	bytes, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".firewall-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(bytes); err != nil {
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
	return os.Rename(f.Name(), m.path)
}
func HostAddresses() ([]netip.Addr, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	result := []netip.Addr{}
	for _, entry := range addresses {
		prefix, err := netip.ParsePrefix(entry.String())
		if err == nil {
			result = append(result, prefix.Addr().Unmap())
		}
	}
	return result, nil
}
func chain(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "HPF_" + hex.EncodeToString(sum[:8])
}
func Render(state State, hosts []netip.Addr, v6 bool) (string, error) {
	var out strings.Builder
	out.WriteString("*filter\n:HIVEPANEL-FW - [0:0]\n-F HIVEPANEL-FW\n")
	ids := []string{}
	for id := range state.Policies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := state.Policies[id]
		if !p.Enabled {
			continue
		}
		if _, err := Validate(p); err != nil {
			return "", err
		}
		allocs := state.Allocations[id]
		for _, a := range allocs {
			if a.Port < 1 || a.Port > 65535 {
				return "", errors.New("invalid allocation")
			}
			bindings := []netip.Addr{}
			ip, err := netip.ParseAddr(a.IP)
			if err != nil {
				return "", errors.New("invalid allocation IP")
			}
			if ip.Is6() != v6 {
				continue
			}
			if ip.IsUnspecified() {
				for _, host := range hosts {
					if host.Is6() == v6 {
						bindings = append(bindings, host)
					}
				}
			} else if ip.Is6() == v6 {
				bindings = append(bindings, ip)
			}
			for _, host := range bindings {
				for _, protocol := range []string{"tcp", "udp"} {
					selector := fmt.Sprintf("-A HIVEPANEL-FW -p %s -m conntrack --ctdir ORIGINAL --ctorigdst %s --ctorigdstport %d", protocol, host.String(), a.Port)
					for _, r := range p.Rules {
						prefix, _ := netip.ParsePrefix(r.Source)
						if prefix.Addr().Is6() != v6 || (r.Port != 0 && r.Port != a.Port) {
							continue
						}
						target := "RETURN"
						if r.Action == "deny" {
							target = "DROP"
						}
						fmt.Fprintf(&out, "%s -s %s -j %s\n", selector, r.Source, target)
					}
					target := "RETURN"
					if p.Default == "deny" {
						target = "DROP"
					}
					fmt.Fprintf(&out, "%s -j %s\n", selector, target)
				}
			}
		}
	}
	out.WriteString("-A HIVEPANEL-FW -j RETURN\nCOMMIT\n")
	return out.String(), nil
}
func (m *Manager) applyLocked(ctx context.Context) error {
	hosts, err := m.Hosts()
	if err != nil {
		return err
	}
	for _, family := range []struct {
		command string
		v6      bool
	}{{"iptables", false}, {"ip6tables", true}} {
		// DOCKER-USER is required for IPv4 Docker bridge traffic. IPv6 may not have a
		// Docker chain when Docker IPv6 is disabled; INPUT remains protected there.
		_, dockerErr := m.runner.Run(ctx, family.command, []string{"-w", "5", "-S", "DOCKER-USER"}, "")
		if dockerErr != nil && !family.v6 {
			return errors.New("Docker's iptables DOCKER-USER chain is unavailable; this node's firewall backend is not supported")
		}
		text, err := Render(m.state, hosts, family.v6)
		if err != nil {
			return err
		}
		if _, err = m.runner.Run(ctx, family.command+"-restore", []string{"--wait", "5", "--noflush"}, text); err != nil {
			return errors.New("could not apply node firewall rules; check iptables/ip6tables support and Worker privileges")
		}
		parents := []string{"INPUT"}
		if dockerErr == nil {
			parents = append(parents, "DOCKER-USER")
		}
		for _, parent := range parents {
			args := []string{"-w", "5", "-C", parent, "-j", "HIVEPANEL-FW"}
			if _, err := m.runner.Run(ctx, family.command, args, ""); err != nil {
				if _, err = m.runner.Run(ctx, family.command, []string{"-w", "5", "-I", parent, "1", "-j", "HIVEPANEL-FW"}, ""); err != nil {
					return errors.New("could not attach HivePanel firewall chain")
				}
			}
		}
	}
	return nil
}
