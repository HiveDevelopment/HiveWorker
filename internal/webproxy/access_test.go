package webproxy

import (
	"hivepanel-worker/internal/firewall"
	"strings"
	"testing"
)

func TestAccessRulesAreOrderedAndScopedToCellPort(t *testing.T) {
	var out strings.Builder
	p := firewall.Policy{Enabled: true, Default: "deny", Rules: []firewall.Rule{{Action: "allow", Source: "192.0.2.0/24", Port: 8080}, {Action: "deny", Source: "198.51.100.1/32", Port: 25565}}}
	writeHTTPProxy(&out, Route{CellID: "test", Port: 8080, MaxBodyMB: 32}, p)
	value := out.String()
	if !strings.Contains(value, "allow 192.0.2.0/24;\ndeny all;") {
		t.Fatal(value)
	}
	if strings.Contains(value, "198.51.100.1") {
		t.Fatal("applied another service's rule")
	}
	if strings.Index(value, "location /") > strings.Index(value, "allow 192.0.2.") {
		t.Fatal("ACL outside application location")
	}
}
