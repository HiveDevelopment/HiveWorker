package webproxy

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (m *Manager) certificate(r Route) (*x509.Certificate, error) {
	data, err := os.ReadFile(filepath.Join(m.root, "certificates", "live", r.ID, "fullchain.pem"))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("invalid certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := cert.VerifyHostname(r.Domain); err != nil {
		return nil, err
	}
	return cert, nil
}
func (m *Manager) certificateReady(r Route) bool {
	cert, err := m.certificate(r)
	return err == nil && cert.NotAfter.After(time.Now())
}
func (m *Manager) RequestCertificate(cell, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, index, err := m.findLocked(cell, id)
	if err != nil {
		return err
	}
	if !m.state.Config.Enabled || !r.Verified || !r.HTTPS || r.Protocol != "http" {
		return errors.New("verify an HTTP hostname and enable HTTPS before requesting a certificate")
	}
	if m.state.Config.ACMEEmail == "" {
		return errors.New("node ACME email is not configured")
	}
	if m.jobs[id] != nil {
		return errors.New("certificate request is already running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	m.jobs[id] = cancel
	previous := r
	r.CertificateState = "issuing"
	r.Error = ""
	m.state.Routes[index] = r
	if err := atomicJSON(m.statePath(), m.state); err != nil {
		m.state.Routes[index] = previous
		cancel()
		delete(m.jobs, id)
		return err
	}
	config := m.state.Config
	go m.issue(ctx, r, config)
	return nil
}
func (m *Manager) issue(ctx context.Context, r Route, c Config) {
	m.certificateMu.Lock()
	defer m.certificateMu.Unlock()
	var err error
	var output []byte
	if ctx.Err() != nil {
		err = ctx.Err()
	} else {
		err = m.ensureImage(ctx, c.CertbotImage)
	}
	if err == nil {
		name := "hivepanel-cert-" + r.ID
		user, _, _, identityErr := m.Identity()
		err = identityErr
		if err == nil {
			err = m.removeCertificateContainer(ctx, name, r.ID)
		}
		if err == nil {
			args := []string{"run", "--rm", "--name", name, "--user", user, "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--read-only", "--tmpfs", "/tmp:rw,mode=1777", "--pids-limit", "128", "--memory", "512m", "--label", "hivepanel.cert_route=" + r.ID, "--label", "hivepanel.proxy_node=" + m.node, "-v", filepath.Join(m.root, "certificates") + ":/etc/letsencrypt:rw,z", "-v", filepath.Join(m.root, "challenges") + ":/var/www/acme:rw,z", "--entrypoint", "certbot", c.CertbotImage, "certonly", "--non-interactive", "--agree-tos", "--email", c.ACMEEmail, "--webroot", "--webroot-path", "/var/www/acme", "--cert-name", r.ID, "--domain", r.Domain, "--keep-until-expiring", "--work-dir", "/etc/letsencrypt/work", "--logs-dir", "/etc/letsencrypt/logs"}
			output, err = m.runner.Run(ctx, args...)
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = m.removeCertificateContainer(cleanup, name, r.ID)
			cancel()
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs[r.ID] == nil {
		return
	}
	m.jobs[r.ID]()
	delete(m.jobs, r.ID)
	current, index, findErr := m.findLocked(r.CellID, r.ID)
	if findErr != nil {
		return
	}
	if err == nil {
		_, uid, gid, identityErr := m.Identity()
		if identityErr != nil {
			err = identityErr
		} else {
			err = m.prepare(uid, gid)
		}
	}
	if err == nil {
		cert, readErr := m.certificate(current)
		if readErr != nil {
			err = readErr
		} else {
			expires := cert.NotAfter
			current.CertificateExpires = &expires
			current.CertificateState = "ready"
			current.Error = ""
		}
	}
	if err != nil {
		current.CertificateState = "failed"
		current.Error = certificateError(string(output))
		details := string(output)
		if len(details) > 4000 {
			details = details[len(details)-4000:]
		}
		log.Printf("Certificate request failed for %s (%s): %v; %s", r.Domain, r.ID, err, details)
	}
	m.state.Routes[index] = current
	applyCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if applyErr := m.applyLocked(applyCtx, m.state); applyErr != nil {
		current.Error = "Certificate state saved, but proxy reload failed. Ask an administrator to check the node proxy."
		m.state.Routes[index] = current
		m.lastError = applyErr.Error()
	}
	_ = atomicJSON(m.statePath(), m.state)
}
func certificateError(output string) string {
	lower := strings.ToLower(output)
	if strings.Contains(lower, "nxdomain") {
		return "DNS lookup failed. Add the hostname's A/AAAA or CNAME record before retrying."
	}
	if strings.Contains(lower, "unauthorized") || strings.Contains(lower, "invalid response") || strings.Contains(lower, "timeout") || strings.Contains(lower, "connection refused") {
		return "Certificate validation could not reach this node. Check DNS and public port 80, then retry."
	}
	if strings.Contains(lower, "too many") || strings.Contains(lower, "rate limit") {
		return "The certificate authority rate limit was reached. Wait before retrying."
	}
	return "Certificate request failed. Check DNS, public port 80 and node connectivity; an administrator can inspect the Worker journal."
}
func (m *Manager) StartRenewal(ctx context.Context) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for {
		if m.Config().Enabled {
			for _, r := range m.Routes("") {
				if r.Verified && r.HTTPS && r.Protocol == "http" && r.CertificateExpires != nil && time.Until(*r.CertificateExpires) < 30*24*time.Hour {
					_ = m.RequestCertificate(r.CellID, r.ID)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) PublicRoutes(cell string) map[string]any {
	routes := m.Routes(cell)
	c := m.Config()
	return map[string]any{"enabled": c.Enabled, "dns_address": c.DNSAddress, "http_port": c.HTTPPort, "https_port": c.HTTPSPort, "tcp_ports": c.TCPPorts, "udp_ports": c.UDPPorts, "https_available": c.ACMEEmail != "", "routes": routes, "verification_prefix": "hivepanel-verification=", "verification_label": "_hivepanel"}
}
func (m *Manager) DNSInstructions(r Route) map[string]string {
	c := m.Config()
	record := "CNAME"
	if strings.Contains(c.DNSAddress, ":") || isIP(c.DNSAddress) {
		record = "A / AAAA"
	}
	result := map[string]string{"record_type": record, "target": c.DNSAddress, "verification_name": "_hivepanel." + r.Domain, "verification_value": "hivepanel-verification=" + r.VerificationToken}
	if r.Protocol == "tcp" {
		result["minecraft_srv"] = fmt.Sprintf("0 5 %d %s", r.PublicPort, r.Domain)
	}
	return result
}
func isIP(value string) bool { // Config validates the value; avoid guessing hostnames from punctuation.
	return strings.Count(value, ".") == 3 && func() bool {
		for _, char := range value {
			if (char < '0' || char > '9') && char != '.' {
				return false
			}
		}
		return true
	}()
}

func (m *Manager) removeCertificateContainer(ctx context.Context, name, id string) error {
	data, err := m.runner.Run(ctx, "inspect", name)
	if err != nil {
		if strings.Contains(string(data), "No such") || strings.Contains(string(data), "not found") {
			return nil
		}
		return errors.New("could not inspect certificate container")
	}
	var containers []struct {
		Config struct{ Labels map[string]string }
	}
	if json.Unmarshal(data, &containers) != nil || len(containers) != 1 || containers[0].Config.Labels["hivepanel.cert_route"] != id || containers[0].Config.Labels["hivepanel.proxy_node"] != m.node {
		return errors.New("refusing to remove an unmanaged certificate container")
	}
	_, err = m.runner.Run(ctx, "rm", "-f", name)
	return err
}
