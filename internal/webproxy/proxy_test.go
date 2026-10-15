package webproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeDocker struct {
	containers           map[string]map[string]string
	failTest, failReload bool
	calls                [][]string
}

func (f *fakeDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{}, args...))
	if args[0] == "image" || args[0] == "pull" {
		return nil, nil
	}
	if args[0] == "inspect" {
		name := args[len(args)-1]
		labels, ok := f.containers[name]
		if !ok {
			return []byte("No such container"), errors.New("missing")
		}
		if len(args) > 2 {
			return []byte("true"), nil
		}
		data, _ := json.Marshal([]any{map[string]any{"State": map[string]any{"Running": true}, "Config": map[string]any{"Labels": labels}}})
		return data, nil
	}
	if args[0] == "rm" {
		delete(f.containers, args[len(args)-1])
		return nil, nil
	}
	if args[0] == "exec" {
		if f.failReload {
			f.failReload = false
			return nil, errors.New("reload failed")
		}
		return nil, nil
	}
	if args[0] == "run" {
		if args[len(args)-1] == "-V" {
			return []byte("--with-stream=dynamic"), nil
		}
		if containsString(args, "-t") && f.failTest {
			return nil, errors.New("config rejected")
		}
		if containsString(args, "-d") {
			labels := map[string]string{}
			name := ""
			for i, a := range args {
				if a == "--label" {
					parts := strings.SplitN(args[i+1], "=", 2)
					labels[parts[0]] = parts[1]
				}
				if a == "--name" {
					name = args[i+1]
				}
			}
			f.containers[name] = labels
		}
		return nil, nil
	}
	return nil, errors.New("unexpected Docker command")
}
func containsString(args []string, value string) bool {
	for _, s := range args {
		if s == value {
			return true
		}
	}
	return false
}
func proxyFixture(t *testing.T) (*Manager, *fakeDocker) {
	t.Helper()
	f := &fakeDocker{containers: map[string]map[string]string{}}
	m, err := New(t.TempDir(), "hive-network", "node", f)
	if err != nil {
		t.Fatal(err)
	}
	m.Identity = func() (string, int, int, error) { return "1000:1000", os.Getuid(), os.Getgid(), nil }
	c := Defaults()
	c.Enabled = true
	c.DNSAddress = "web.example.com"
	c.ACMEEmail = "admin@example.com"
	c.TCPPorts = []int{25566, 25567}
	c.UDPPorts = []int{19133}
	if err := m.Configure(context.Background(), c, []int{25565, 8123}); err != nil {
		t.Fatal(err)
	}
	return m, f
}
func routeFixture(id, domain string) Route {
	return Route{ID: id, CellID: "cell", Domain: domain, Port: 8123, Protocol: "http", MaxBodyMB: 32}
}
func TestDomainsRejectInjectionAndNormalize(t *testing.T) {
	for _, bad := range []string{"127.0.0.1", "https://example.com", "*.example.com", "a.example.com;return 200", "localhost", "a..com", "example.com/", "-bad.example"} {
		if _, err := Domain(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if domain, err := Domain(" WWW.Example.com. "); err != nil || domain != "www.example.com" {
		t.Fatalf("normalization %s %v", domain, err)
	}
}
func TestHTTPRenderingRequiresOwnershipAndProtectsTLS(t *testing.T) {
	c := Defaults()
	c.ACMEEmail = "admin@example.com"
	c.HTTPSPort = 8443
	r := routeFixture("route", "www.example.com")
	s := state{Config: c, Routes: []Route{r}}
	text, err := Render(s, func(Route) bool { return false })
	if err != nil || strings.Contains(text, "server_name www") {
		t.Fatal("unverified domain routed", err)
	}
	s.Routes[0].Verified = true
	s.Routes[0].HTTPS = true
	s.Routes[0].Redirect = true
	text, _ = Render(s, func(Route) bool { return false })
	if strings.Contains(text, "proxy_pass http") || !strings.Contains(text, "return 503") {
		t.Fatal("TLS-pending route served plaintext")
	}
	text, _ = Render(s, func(Route) bool { return true })
	for _, wanted := range []string{"https://www.example.com:8443$request_uri", "ssl_reject_handshake on", "proxy_set_header Upgrade", "proxy_set_header X-Forwarded-For $remote_addr", "hivepanel-cell-cell:8123"} {
		if !strings.Contains(text, wanted) {
			t.Error("missing", wanted)
		}
	}
}
func TestDomainVerificationAndPortGuards(t *testing.T) {
	m, _ := proxyFixture(t)
	r, err := m.Put(context.Background(), routeFixture("one", "www.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Verified || len(r.VerificationToken) < 32 {
		t.Fatal("missing challenge")
	}
	m.LookupTXT = func(context.Context, string) ([]string, error) { return []string{"wrong"}, nil }
	if _, err = m.Verify(context.Background(), "cell", "one"); err == nil {
		t.Fatal("wrong TXT accepted")
	}
	m.LookupTXT = func(_ context.Context, name string) ([]string, error) {
		if name != "_hivepanel.www.example.com" {
			t.Fatal(name)
		}
		return []string{"hivepanel-verification=" + r.VerificationToken}, nil
	}
	if _, err = m.Verify(context.Background(), "cell", "one"); err != nil {
		t.Fatal(err)
	}
	if err = m.CheckCellPorts("cell", []int{25565}); err == nil {
		t.Fatal("referenced port removed")
	}
	if err = m.CheckCellPorts("other", []int{80}); err == nil {
		t.Fatal("proxy port claimed")
	}
	if _, err = m.Put(context.Background(), routeFixture("two", "www.example.com")); err == nil {
		t.Fatal("duplicate domain accepted")
	}
	if err = m.Delete(context.Background(), "other", "one"); err == nil {
		t.Fatal("cross-cell route deletion allowed")
	}
}
func TestStreamsReserveDedicatedPorts(t *testing.T) {
	m, _ := proxyFixture(t)
	r := routeFixture("one", "play.example.com")
	r.Protocol = "tcp"
	r.Port = 25565
	r.PublicPort = 25566
	if _, err := m.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.ID = "two"
	r.Domain = "another.example.com"
	if _, err := m.Put(context.Background(), r); err == nil {
		t.Fatal("stream port reused")
	}
	r.PublicPort = 25568
	if _, err := m.Put(context.Background(), r); err == nil {
		t.Fatal("unoffered port accepted")
	}
	if m.state.StreamModule == "" {
		t.Fatal("dynamic stream module state lost")
	}
}
func TestRejectedConfigurationRetainsRoutesAndFiles(t *testing.T) {
	m, f := proxyFixture(t)
	r, _ := m.Put(context.Background(), routeFixture("one", "www.example.com"))
	m.LookupTXT = func(context.Context, string) ([]string, error) {
		return []string{"hivepanel-verification=" + r.VerificationToken}, nil
	}
	before, _ := os.ReadFile(filepath.Join(m.root, "nginx.conf"))
	f.failTest = true
	if _, err := m.Verify(context.Background(), "cell", "one"); err == nil {
		t.Fatal("test failure ignored")
	}
	after, _ := os.ReadFile(filepath.Join(m.root, "nginx.conf"))
	if string(before) != string(after) || m.Routes("cell")[0].Verified {
		t.Fatal("invalid candidate was committed")
	}
	f.failTest = false
	f.failReload = true
	if _, err := m.Verify(context.Background(), "cell", "one"); err == nil {
		t.Fatal("reload failure ignored")
	}
	after, _ = os.ReadFile(filepath.Join(m.root, "nginx.conf"))
	if string(before) != string(after) {
		t.Fatal("active config not restored")
	}
}
func TestUnmanagedContainersAreNeverRemoved(t *testing.T) {
	m, f := proxyFixture(t)
	f.containers[m.name()] = map[string]string{"other": "true"}
	c := m.Config()
	c.Enabled = false
	if err := m.Configure(context.Background(), c, nil); err == nil {
		t.Fatal("unmanaged proxy removed")
	}
	f.containers["hivepanel-cert-one"] = map[string]string{"hivepanel.cert_route": "one"}
	if err := m.removeCertificateContainer(context.Background(), "hivepanel-cert-one", "one"); err == nil {
		t.Fatal("unmanaged certificate container removed")
	}
	if _, ok := f.containers["hivepanel-cert-one"]; !ok {
		t.Fatal("unmanaged cert container lost")
	}
}
func TestDeleteCellRestoresProxyWhenCellRemovalFails(t *testing.T) {
	m, _ := proxyFixture(t)
	r, _ := m.Put(context.Background(), routeFixture("one", "www.example.com"))
	m.LookupTXT = func(context.Context, string) ([]string, error) {
		return []string{"hivepanel-verification=" + r.VerificationToken}, nil
	}
	if _, err := m.Verify(context.Background(), "cell", "one"); err != nil {
		t.Fatal(err)
	}
	err := m.DeleteCell(context.Background(), "cell", func() error { return errors.New("still running") })
	if err == nil || len(m.Routes("cell")) != 1 {
		t.Fatal("route removed before failed Cell deletion")
	}
	if err := m.DeleteCell(context.Background(), "cell", func() error { return nil }); err != nil || len(m.Routes("cell")) != 0 {
		t.Fatal("successful Cell deletion retained route", err)
	}
}

func TestAllHTTPTemporaryPathsAreWritable(t *testing.T) {
	text, err := Render(state{Config: Defaults()}, func(Route) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	for _, directive := range []string{"client_body_temp_path", "proxy_temp_path", "fastcgi_temp_path", "uwsgi_temp_path", "scgi_temp_path"} {
		if !strings.Contains(text, directive+" /tmp/") {
			t.Errorf("%s must use the proxy's writable tmpfs", directive)
		}
	}
}
