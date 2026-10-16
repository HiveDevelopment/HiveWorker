package firewall

import (
	"context"
	"errors"
	"hivepanel-worker/internal/allocation"
	"net/netip"
	"strings"
	"testing"
)

type fakeRunner struct {
	fail  bool
	calls []string
}

func (r *fakeRunner) Run(_ context.Context, cmd string, args []string, input string) ([]byte, error) {
	r.calls = append(r.calls, cmd+" "+strings.Join(args, " ")+"\n"+input)
	if r.fail && cmd == "ip6tables-restore" {
		r.fail = false
		return nil, errors.New("test failure")
	}
	return nil, nil
}
func TestValidationAndOriginalDestination(t *testing.T) {
	for _, source := range []string{"1.2.3.4;DROP", "::ffff:1.2.3.4", "example.com"} {
		if _, err := Validate(Policy{Default: "allow", Rules: []Rule{{Source: source, Action: "deny"}}}); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
	policy, err := Validate(Policy{Enabled: true, Default: "deny", Rules: []Rule{{Source: "192.0.2.5", Action: "allow", Port: 25565}}})
	if err != nil {
		t.Fatal(err)
	}
	state := State{Policies: map[string]Policy{"a": policy}, Allocations: map[string][]allocation.Allocation{"a": {{IP: "0.0.0.0", Port: 25565}}}}
	rendered, err := Render(state, []netip.Addr{netip.MustParseAddr("10.0.0.5")}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"--ctdir ORIGINAL --ctorigdst 10.0.0.5 --ctorigdstport 25565", "-s 192.0.2.5/32 -j RETURN", "-p udp", "-j DROP"} {
		if !strings.Contains(rendered, part) {
			t.Fatalf("missing %s", part)
		}
	}
	if strings.Contains(rendered, "-F DOCKER") {
		t.Fatal("flushed Docker")
	}
}
func TestFailedApplyRestoresPersistedPolicy(t *testing.T) {
	r := &fakeRunner{}
	m, err := New(t.TempDir(), r)
	if err != nil {
		t.Fatal(err)
	}
	m.Hosts = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil }
	ctx := context.Background()
	if err = m.Sync(ctx, "a", []allocation.Allocation{{IP: "0.0.0.0", Port: 25565}}); err != nil {
		t.Fatal(err)
	}
	p := Policy{Enabled: true, Default: "deny"}
	if err = m.Set(ctx, "a", p); err != nil {
		t.Fatal(err)
	}
	r.fail = true
	if err = m.Set(ctx, "a", Policy{Enabled: true, Default: "allow"}); err == nil {
		t.Fatal("failure hidden")
	}
	if m.Policy("a").Default != "deny" {
		t.Fatal("policy was not rolled back")
	}
	reload, err := New(strings.TrimSuffix(m.path, "/firewall.json"), r)
	if err != nil || reload.Policy("a").Default != "deny" {
		t.Fatal("persisted policy changed")
	}
}
