package webproxy

import (
	"context"
	"encoding/json"
	"errors"
	"hivepanel-worker/internal/firewall"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	Enabled        bool     `json:"enabled"`
	BindIP         string   `json:"bind_ip"`
	HTTPPort       int      `json:"http_port"`
	HTTPSPort      int      `json:"https_port"`
	DNSAddress     string   `json:"dns_address"`
	ACMEEmail      string   `json:"acme_email"`
	TCPPorts       []int    `json:"tcp_ports"`
	UDPPorts       []int    `json:"udp_ports"`
	TrustedProxies []string `json:"trusted_proxies"`
	Image          string   `json:"image"`
	CertbotImage   string   `json:"certbot_image"`
}
type Route struct {
	ID                 string     `json:"id"`
	CellID             string     `json:"cell_id"`
	Domain             string     `json:"domain"`
	Port               int        `json:"port"`
	Protocol           string     `json:"protocol"`
	PublicPort         int        `json:"public_port"`
	HTTPS              bool       `json:"https"`
	Redirect           bool       `json:"redirect"`
	Verified           bool       `json:"verified"`
	VerificationToken  string     `json:"verification_token"`
	CertificateState   string     `json:"certificate_state"`
	CertificateExpires *time.Time `json:"certificate_expires,omitempty"`
	Error              string     `json:"error"`
	MaxBodyMB          int        `json:"max_body_mb"`
}
type state struct {
	Policies     map[string]firewall.Policy `json:"policies,omitempty"`
	Config       Config                     `json:"config"`
	Routes       []Route                    `json:"routes"`
	StreamModule string                     `json:"stream_module"`
}
type Runner interface {
	Run(context.Context, ...string) ([]byte, error)
}
type DockerRunner struct{}

func (DockerRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

var labelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,80}$`)
var imagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./_:@-]{1,250}$`)

func Domain(raw string) (string, error) {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if len(domain) > 253 || net.ParseIP(domain) != nil || !strings.Contains(domain, ".") {
		return "", errors.New("enter a hostname such as www.example.com, without a URL, path or wildcard")
	}
	for _, label := range strings.Split(domain, ".") {
		if !labelPattern.MatchString(label) {
			return "", errors.New("invalid hostname; use ASCII or punycode")
		}
	}
	return domain, nil
}
func Defaults() Config {
	return Config{BindIP: "0.0.0.0", HTTPPort: 80, HTTPSPort: 443, Image: "nginx:stable", CertbotImage: "certbot/certbot:latest", TCPPorts: []int{}, UDPPorts: []int{}, TrustedProxies: []string{}}
}
func ValidateConfig(c Config) error {
	if net.ParseIP(c.BindIP) == nil || c.HTTPPort < 1 || c.HTTPPort > 65535 || c.HTTPSPort < 1 || c.HTTPSPort > 65535 || c.HTTPPort == c.HTTPSPort {
		return errors.New("choose a bind IP and distinct valid HTTP/HTTPS ports")
	}
	if !imagePattern.MatchString(c.Image) || !imagePattern.MatchString(c.CertbotImage) {
		return errors.New("invalid proxy image reference")
	}
	if c.Enabled {
		if net.ParseIP(c.DNSAddress) == nil {
			if _, err := Domain(c.DNSAddress); err != nil {
				return errors.New("set the node's public DNS target or IP")
			}
		}
	}
	if len(c.ACMEEmail) > 254 || strings.ContainsAny(c.ACMEEmail, "\r\n") || (c.ACMEEmail != "" && !strings.Contains(c.ACMEEmail, "@")) {
		return errors.New("invalid ACME account email")
	}
	for _, ports := range [][]int{c.TCPPorts, c.UDPPorts} {
		if len(ports) > 32 {
			return errors.New("maximum 32 proxy ports per protocol")
		}
		seen := map[int]bool{}
		for _, p := range ports {
			if p < 1024 || p > 65535 || p == c.HTTPPort || p == c.HTTPSPort || seen[p] {
				return errors.New("stream ports must be distinct, between 1024 and 65535, and separate from HTTP/HTTPS")
			}
			seen[p] = true
		}
	}
	for _, cidr := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return errors.New("trusted proxies must be CIDR ranges")
		}
	}
	return nil
}
func ValidateRoute(r Route, c Config) error {
	if !idPattern.MatchString(r.ID) || !idPattern.MatchString(r.CellID) {
		return errors.New("invalid route or Cell identifier")
	}
	if _, err := Domain(r.Domain); err != nil {
		return err
	}
	if r.Port < 1 || r.Port > 65535 || r.MaxBodyMB < 1 || r.MaxBodyMB > 128 {
		return errors.New("invalid allocation port or request size")
	}
	switch r.Protocol {
	case "http":
		if r.PublicPort != 0 {
			return errors.New("HTTP routes use the node's configured HTTP/HTTPS ports")
		}
		if r.HTTPS && c.ACMEEmail == "" {
			return errors.New("an administrator must configure the ACME account email before enabling HTTPS")
		}
	case "tcp", "udp":
		ports := c.TCPPorts
		if r.Protocol == "udp" {
			ports = c.UDPPorts
		}
		if !contains(ports, r.PublicPort) {
			return errors.New("choose a dedicated proxy port offered by this node")
		}
		if r.HTTPS {
			return errors.New("HTTP certificates do not apply to raw TCP/UDP routes")
		}
	default:
		return errors.New("unsupported proxy protocol")
	}
	return nil
}
func contains(values []int, value int) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func atomicJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
