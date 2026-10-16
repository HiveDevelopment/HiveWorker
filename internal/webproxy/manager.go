package webproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hivepanel-worker/internal/firewall"
	"hivepanel-worker/internal/identity"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	mu                  sync.Mutex
	certificateMu       sync.Mutex
	root, network, node string
	state               state
	runner              Runner
	Identity            func() (string, int, int, error)
	LookupTXT           func(context.Context, string) ([]string, error)
	jobs                map[string]context.CancelFunc
	lastError           string
}

func New(data, network, node string, runner Runner) (*Manager, error) {
	if runner == nil {
		runner = DockerRunner{}
	}
	m := &Manager{root: filepath.Join(data, "web-proxy"), network: network, node: node, runner: runner, Identity: identity.Ensure, jobs: map[string]context.CancelFunc{}, LookupTXT: func(ctx context.Context, name string) ([]string, error) {
		return net.DefaultResolver.LookupTXT(ctx, name)
	}}
	m.state.Config = Defaults()
	m.state.Routes = []Route{}
	bytes, err := os.ReadFile(m.statePath())
	if err == nil {
		if err = json.Unmarshal(bytes, &m.state); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := ValidateConfig(m.state.Config); err != nil {
		return nil, err
	}
	for i := range m.state.Routes {
		if m.state.Routes[i].CertificateState == "issuing" {
			m.state.Routes[i].CertificateState = "failed"
			m.state.Routes[i].Error = "Certificate request interrupted by Worker restart. Retry after checking DNS."
		}
	}
	return m, nil
}
func (m *Manager) statePath() string { return filepath.Join(m.root, "state.json") }
func (m *Manager) name() string      { return "hivepanel-node-proxy" }
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.state.Config
	c.TCPPorts = append([]int{}, c.TCPPorts...)
	c.UDPPorts = append([]int{}, c.UDPPorts...)
	c.TrustedProxies = append([]string{}, c.TrustedProxies...)
	return c
}
func (m *Manager) Routes(cell string) []Route {
	m.mu.Lock()
	defer m.mu.Unlock()
	routes := []Route{}
	for _, r := range m.state.Routes {
		if cell == "" || r.CellID == cell {
			routes = append(routes, r)
		}
	}
	return routes
}
func (m *Manager) ReservedPorts() []int {
	c := m.Config()
	if !c.Enabled {
		return nil
	}
	return append(append([]int{c.HTTPPort, c.HTTPSPort}, c.TCPPorts...), c.UDPPorts...)
}
func (m *Manager) CheckCellPorts(cell string, ports []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.state.Config
	if c.Enabled {
		reserved := append(append([]int{c.HTTPPort, c.HTTPSPort}, c.TCPPorts...), c.UDPPorts...)
		for _, p := range ports {
			if contains(reserved, p) {
				return fmt.Errorf("allocation port %d is reserved by the node proxy", p)
			}
		}
	}
	for _, r := range m.state.Routes {
		if r.CellID == cell && !contains(ports, r.Port) {
			return errors.New("remove or retarget domain/proxy routes before releasing their allocation")
		}
	}
	return nil
}
func (m *Manager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	running := false
	if m.state.Config.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if data, err := m.runner.Run(ctx, "inspect", "--format", "{{.State.Running}}", m.name()); err == nil {
			running = strings.TrimSpace(string(data)) == "true"
		}
	}
	return map[string]any{"config": m.state.Config, "running": running, "error": m.lastError, "routes": append([]Route{}, m.state.Routes...)}
}
func (m *Manager) Configure(ctx context.Context, c Config, allocated []int) error {
	if err := ValidateConfig(c); err != nil {
		return err
	}
	if c.Enabled && (m.network == "" || m.network == "host" || m.network == "bridge" || m.network == "none") {
		return errors.New("configure a user-defined Docker network on the Worker before enabling the proxy")
	}
	reserved := append(append([]int{c.HTTPPort, c.HTTPSPort}, c.TCPPorts...), c.UDPPorts...)
	if c.Enabled {
		for _, port := range allocated {
			if contains(reserved, port) {
				return fmt.Errorf("proxy port %d is assigned to a Cell; choose a free public port", port)
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.state
	next.Config = c
	for _, route := range next.Routes {
		if err := ValidateRoute(route, c); err != nil {
			return errors.New("remove routes using ports/settings that this change would disable")
		}
	}
	if err := m.applyLocked(ctx, next); err != nil {
		m.lastError = err.Error()
		return err
	}
	next.StreamModule = m.state.StreamModule
	m.state = next
	m.lastError = ""
	return atomicJSON(m.statePath(), m.state)
}
func (m *Manager) Restore(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.Config.Enabled {
		return nil
	}
	if err := m.applyLocked(ctx, m.state); err != nil {
		m.lastError = err.Error()
		return err
	}
	return nil
}
func (m *Manager) Put(ctx context.Context, route Route) (Route, error) {
	domain, err := Domain(route.Domain)
	if err != nil {
		return Route{}, err
	}
	route.Domain = domain
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.Config.Enabled {
		return Route{}, errors.New("the node proxy is disabled; ask an administrator to enable it")
	}
	if err := ValidateRoute(route, m.state.Config); err != nil {
		return Route{}, err
	}
	next := m.state
	next.Routes = append([]Route{}, m.state.Routes...)
	index := -1
	for i, old := range next.Routes {
		if old.ID == route.ID {
			if old.CellID != route.CellID || old.Domain != route.Domain {
				return Route{}, errors.New("route belongs to a different Cell or hostname")
			}
			index = i
			route.Verified = old.Verified
			route.VerificationToken = old.VerificationToken
			route.CertificateState = old.CertificateState
			route.CertificateExpires = old.CertificateExpires
			route.Error = old.Error
			continue
		}
		if old.Domain == route.Domain {
			return Route{}, errors.New("hostname is already assigned on this node")
		}
		if route.Protocol != "http" && old.Protocol == route.Protocol && old.PublicPort == route.PublicPort {
			return Route{}, errors.New("dedicated proxy port is already in use")
		}
	}
	if index < 0 {
		count := 0
		for _, r := range next.Routes {
			if r.CellID == route.CellID {
				count++
			}
		}
		if count >= 50 || len(next.Routes) >= 1000 {
			return Route{}, errors.New("domain route limit reached")
		}
		route.Verified = false
		route.CertificateState = "none"
		route.Error = ""
		route.CertificateExpires = nil
		token := make([]byte, 24)
		if _, err := rand.Read(token); err != nil {
			return Route{}, err
		}
		route.VerificationToken = hex.EncodeToString(token)
		next.Routes = append(next.Routes, route)
	} else {
		next.Routes[index] = route
	}
	if err := m.commitLocked(ctx, next, route.Verified); err != nil {
		return Route{}, err
	}
	return route, nil
}
func (m *Manager) Verify(ctx context.Context, cell, id string) (Route, error) {
	m.mu.Lock()
	route, _, err := m.findLocked(cell, id)
	m.mu.Unlock()
	if err != nil {
		return Route{}, err
	}
	records, err := m.LookupTXT(ctx, "_hivepanel."+route.Domain)
	if err != nil {
		return Route{}, errors.New("verification TXT record is not available yet")
	}
	verified := false
	for _, record := range records {
		if record == "hivepanel-verification="+route.VerificationToken {
			verified = true
		}
	}
	if !verified {
		return Route{}, errors.New("TXT verification value does not match; add the displayed record and wait for DNS propagation")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, index, err := m.findLocked(cell, id)
	if err != nil {
		return Route{}, err
	}
	if current.VerificationToken != route.VerificationToken {
		return Route{}, errors.New("domain verification changed; retry")
	}
	next := m.state
	next.Routes = append([]Route{}, m.state.Routes...)
	current.Verified = true
	next.Routes[index] = current
	if err := m.commitLocked(ctx, next, true); err != nil {
		return Route{}, err
	}
	return current, nil
}
func (m *Manager) findLocked(cell, id string) (Route, int, error) {
	for i, r := range m.state.Routes {
		if r.ID == id && (cell == "" || r.CellID == cell) {
			return r, i, nil
		}
	}
	return Route{}, -1, errors.New("route not found for this Cell")
}
func (m *Manager) Delete(ctx context.Context, cell, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, index, err := m.findLocked(cell, id)
	if err != nil {
		return err
	}
	next := m.state
	next.Routes = append([]Route{}, m.state.Routes[:index]...)
	next.Routes = append(next.Routes, m.state.Routes[index+1:]...)
	if err := m.commitLocked(ctx, next, true); err != nil {
		return err
	}
	if cancel := m.jobs[id]; cancel != nil {
		cancel()
		delete(m.jobs, id)
	}
	return nil
}
func (m *Manager) DeleteCell(ctx context.Context, cell string, deleteCell func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.state
	next := old
	next.Routes = []Route{}
	for _, r := range old.Routes {
		if r.CellID != cell {
			next.Routes = append(next.Routes, r)
		}
	}
	if len(next.Routes) == len(old.Routes) {
		return deleteCell()
	}
	if err := m.commitLocked(ctx, next, true); err != nil {
		return err
	}
	if err := deleteCell(); err != nil {
		rollback, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if restoreErr := m.commitLocked(rollback, old, true); restoreErr != nil {
			m.lastError = "Cell deletion failed and the domain proxy could not be restored"
		}
		return err
	}

	for _, r := range old.Routes {
		if r.CellID == cell {
			if cancel := m.jobs[r.ID]; cancel != nil {
				cancel()
				delete(m.jobs, r.ID)
			}
		}
	}
	return nil
}
func (m *Manager) ensureImage(ctx context.Context, image string) error {
	if _, err := m.runner.Run(ctx, "image", "inspect", image); err == nil {
		return nil
	}
	if _, err := m.runner.Run(ctx, "pull", image); err != nil {
		return errors.New("could not download proxy/certificate image; check node connectivity")
	}
	return nil
}
func (m *Manager) prepare(uid, gid int) error {
	for _, dir := range []string{m.root, filepath.Join(m.root, "challenges"), filepath.Join(m.root, "certificates")} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return err
		}
	}
	return filepath.Walk(m.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}
func (m *Manager) applyLocked(ctx context.Context, next state) error {
	if !next.Config.Enabled {
		data, err := m.runner.Run(ctx, "inspect", m.name())
		if err != nil {
			if strings.Contains(string(data), "No such") || strings.Contains(string(data), "not found") {
				return nil
			}
			return err
		}
		var inspect []struct {
			Config struct{ Labels map[string]string }
		}
		if json.Unmarshal(data, &inspect) != nil || len(inspect) != 1 || inspect[0].Config.Labels["hivepanel.proxy_node"] != m.node {
			return errors.New("refusing to remove an unmanaged proxy container")
		}
		_, err = m.runner.Run(ctx, "rm", "-f", m.name())
		return err
	}
	if m.network == "" || m.network == "host" || m.network == "bridge" || m.network == "none" {
		return errors.New("node proxy requires a user-defined Docker network")
	}
	user, uid, gid, err := m.Identity()
	if err != nil {
		return err
	}
	if err := m.prepare(uid, gid); err != nil {
		return err
	}
	if err := m.ensureImage(ctx, next.Config.Image); err != nil {
		return err
	}
	if len(next.Config.TCPPorts)+len(next.Config.UDPPorts) > 0 {
		output, err := m.runner.Run(ctx, "run", "--rm", "--entrypoint", "nginx", next.Config.Image, "-V")
		if err != nil || !strings.Contains(string(output), "--with-stream") {
			return errors.New("proxy image does not include Nginx stream support")
		}
		if strings.Contains(string(output), "--with-stream=dynamic") {
			next.StreamModule = "/usr/lib/nginx/modules/ngx_stream_module.so"
		} else {
			next.StreamModule = ""
		}
	}
	if len(next.Config.TCPPorts)+len(next.Config.UDPPorts) == 0 {
		next.StreamModule = ""
	}
	config, err := Render(next, m.certificateReady)
	if err != nil {
		return err
	}
	candidate := filepath.Join(m.root, "candidate.conf")
	if err := os.WriteFile(candidate, []byte(config), 0640); err != nil {
		return err
	}
	if err := os.Chown(candidate, uid, gid); err != nil {
		return err
	}
	testArgs := []string{"run", "--rm", "--user", user, "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--read-only", "--tmpfs", "/tmp:rw,mode=1777", "-v", m.root + ":/etc/hiveproxy:ro,z", "--entrypoint", "nginx", next.Config.Image, "-t", "-c", "/etc/hiveproxy/candidate.conf"}
	if _, err := m.runner.Run(ctx, testArgs...); err != nil {
		return errors.New("generated proxy configuration failed validation; existing routes were retained")
	}
	active := filepath.Join(m.root, "nginx.conf")
	previous, _ := os.ReadFile(active)
	if err := os.Rename(candidate, active); err != nil {
		return err
	}
	if err := m.ensureContainerLocked(ctx, next, user); err != nil {
		if len(previous) > 0 {
			_ = os.WriteFile(active, previous, 0640)
			_, _ = m.runner.Run(ctx, "exec", m.name(), "nginx", "-s", "reload", "-c", "/etc/hiveproxy/nginx.conf")
		}
		return err
	}
	if _, err := m.runner.Run(ctx, "exec", m.name(), "nginx", "-s", "reload", "-c", "/etc/hiveproxy/nginx.conf"); err != nil {
		if len(previous) > 0 {
			_ = os.WriteFile(active, previous, 0640)
			_, _ = m.runner.Run(ctx, "exec", m.name(), "nginx", "-s", "reload", "-c", "/etc/hiveproxy/nginx.conf")
		}
		return errors.New("proxy reload failed; previous configuration was restored")
	}
	m.state.StreamModule = next.StreamModule
	return nil
}
func (m *Manager) ensureContainerLocked(ctx context.Context, next state, user string) error {
	c := next.Config
	signature, _ := json.Marshal([]any{c.Image, c.BindIP, c.HTTPPort, c.HTTPSPort, c.TCPPorts, c.UDPPorts, m.network, user})
	sum := sha256.Sum256(signature)
	hash := hex.EncodeToString(sum[:])
	output, err := m.runner.Run(ctx, "inspect", m.name())
	if err == nil {
		var containers []struct {
			State  struct{ Running bool }
			Config struct{ Labels map[string]string }
		}
		if json.Unmarshal(output, &containers) != nil || len(containers) != 1 || containers[0].Config.Labels["hivepanel.proxy_node"] != m.node {
			return errors.New("a container with this proxy name exists but is not managed by this node")
		}
		if containers[0].State.Running && containers[0].Config.Labels["hivepanel.proxy_config"] == hash {
			return nil
		}
		if _, err := m.runner.Run(ctx, "rm", "-f", m.name()); err != nil {
			return err
		}
	} else if !strings.Contains(string(output), "No such") && !strings.Contains(string(output), "not found") {
		return errors.New("Docker could not inspect the node proxy")
	}
	args := []string{"run", "-d", "--restart", "unless-stopped", "--name", m.name(), "--network", m.network, "--user", user, "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--read-only", "--tmpfs", "/tmp:rw,mode=1777", "--pids-limit", "256", "--memory", "512m", "--label", "hivepanel.proxy_node=" + m.node, "--label", "hivepanel.proxy_config=" + hash, "-v", m.root + ":/etc/hiveproxy:ro,z", "-p", net.JoinHostPort(c.BindIP, fmt.Sprint(c.HTTPPort)) + ":8080/tcp", "-p", net.JoinHostPort(c.BindIP, fmt.Sprint(c.HTTPSPort)) + ":8443/tcp"}
	for _, protocol := range []string{"tcp", "udp"} {
		ports := c.TCPPorts
		if protocol == "udp" {
			ports = c.UDPPorts
		}
		for _, p := range ports {
			args = append(args, "-p", net.JoinHostPort(c.BindIP, fmt.Sprint(p))+":"+fmt.Sprint(p)+"/"+protocol)
		}
	}
	args = append(args, "--entrypoint", "nginx", c.Image, "-c", "/etc/hiveproxy/nginx.conf", "-g", "daemon off;")
	if _, err := m.runner.Run(ctx, args...); err != nil {
		return errors.New("could not start the node proxy; check port conflicts and Docker network")
	}
	return nil
}

// Persist the same route set that was activated; restore both when a state write fails.
func (m *Manager) commitLocked(ctx context.Context, next state, apply bool) error {
	old := m.state
	if apply {
		if err := m.applyLocked(ctx, next); err != nil {
			return err
		}
		next.StreamModule = m.state.StreamModule
	}
	if err := atomicJSON(m.statePath(), next); err != nil {
		if apply {
			rollback, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = m.applyLocked(rollback, old)
			cancel()
		}
		m.state = old
		return errors.New("could not save node proxy state; check the node's storage")
	}
	m.state = next
	return nil
}

func (m *Manager) SetAccessPolicy(ctx context.Context, id string, p firewall.Policy) error {
	p, err := firewall.Validate(p)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.state.Policies[id]; (ok && reflect.DeepEqual(old, p)) || (!ok && !p.Enabled) {
		return nil
	}
	next := m.state
	next.Policies = map[string]firewall.Policy{}
	for id, policy := range m.state.Policies {
		next.Policies[id] = policy
	}
	next.Policies[id] = p
	return m.commitLocked(ctx, next, next.Config.Enabled)
}
