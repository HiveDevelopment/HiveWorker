package webproxy

import (
	"fmt"
	"hivepanel-worker/internal/firewall"
	"strings"
)

func Render(s state, certificateReady func(Route) bool) (string, error) {
	if err := ValidateConfig(s.Config); err != nil {
		return "", err
	}
	var out strings.Builder
	if s.StreamModule != "" {
		if s.StreamModule != "/usr/lib/nginx/modules/ngx_stream_module.so" {
			return "", fmt.Errorf("unexpected Nginx stream module path")
		}
		fmt.Fprintf(&out, "load_module %s;\n", s.StreamModule)
	}
	out.WriteString("worker_processes auto;\npid /tmp/hivepanel-nginx.pid;\nerror_log /dev/stderr warn;\nevents { worker_connections 1024; }\nhttp {\ninclude /etc/nginx/mime.types;\ndefault_type application/octet-stream;\naccess_log /dev/stdout;\nserver_tokens off;\nresolver 127.0.0.11 valid=10s ipv6=off;\nmap $http_upgrade $connection_upgrade { default upgrade; '' close; }\nclient_body_temp_path /tmp/client_body;\nproxy_temp_path /tmp/proxy;\nfastcgi_temp_path /tmp/fastcgi;\nuwsgi_temp_path /tmp/uwsgi;\nscgi_temp_path /tmp/scgi;\n")
	for _, cidr := range s.Config.TrustedProxies {
		fmt.Fprintf(&out, "set_real_ip_from %s;\n", cidr)
	}
	if len(s.Config.TrustedProxies) > 0 {
		out.WriteString("real_ip_header X-Forwarded-For;\nreal_ip_recursive on;\n")
	}
	out.WriteString("server { listen 8080 default_server; server_name _; return 404; }\nserver { listen 8443 ssl default_server; ssl_reject_handshake on; }\n")
	for _, r := range s.Routes {
		if err := ValidateRoute(r, s.Config); err != nil {
			return "", err
		}
		if !r.Verified || r.Protocol != "http" {
			continue
		}
		ready := r.HTTPS && certificateReady(r)
		fmt.Fprintf(&out, "server { listen 8080; server_name %s;\nlocation ^~ /.well-known/acme-challenge/ { root /etc/hiveproxy/challenges; try_files $uri =404; }\n", r.Domain)
		if r.HTTPS && !ready {
			out.WriteString("location / { return 503; }\n")
		} else if ready && r.Redirect {
			port := ""
			if s.Config.HTTPSPort != 443 {
				port = fmt.Sprintf(":%d", s.Config.HTTPSPort)
			}
			fmt.Fprintf(&out, "location / { return 308 https://%s%s$request_uri; }\n", r.Domain, port)
		} else {
			writeHTTPProxy(&out, r, s.Policies[r.CellID])
		}
		out.WriteString("}\n")
		if ready {
			fmt.Fprintf(&out, "server { listen 8443 ssl; server_name %s;\nssl_certificate /etc/hiveproxy/certificates/live/%s/fullchain.pem;\nssl_certificate_key /etc/hiveproxy/certificates/live/%s/privkey.pem;\nssl_protocols TLSv1.2 TLSv1.3;\n", r.Domain, r.ID, r.ID)
			writeHTTPProxy(&out, r, s.Policies[r.CellID])
			out.WriteString("}\n")
		}
	}
	out.WriteString("}\n")
	if len(s.Config.TCPPorts)+len(s.Config.UDPPorts) > 0 {
		out.WriteString("stream { resolver 127.0.0.11 valid=10s ipv6=off;\n")
		for _, r := range s.Routes {
			if !r.Verified || r.Protocol == "http" {
				continue
			}
			udp := ""
			if r.Protocol == "udp" {
				udp = " udp reuseport"
			}
			fmt.Fprintf(&out, "server { listen %d%s;\n", r.PublicPort, udp)
			writeAccess(&out, s.Policies[r.CellID], r.Port)
			fmt.Fprintf(&out, "set $backend hivepanel-cell-%s:%d; proxy_pass $backend; proxy_connect_timeout 10s; proxy_timeout 300s; }\n", r.CellID, r.Port)
		}
		out.WriteString("}\n")
	}
	return out.String(), nil
}
func writeHTTPProxy(out *strings.Builder, r Route, policy firewall.Policy) {
	fmt.Fprintf(out, "client_max_body_size %dm;\nlocation / { set $backend hivepanel-cell-%s:%d; proxy_pass http://$backend;\n", r.MaxBodyMB, r.CellID, r.Port)
	writeAccess(out, policy, r.Port)
	out.WriteString("proxy_http_version 1.1;\nproxy_set_header Host $host;\nproxy_set_header X-Real-IP $remote_addr;\nproxy_set_header X-Forwarded-For $remote_addr;\nproxy_set_header X-Forwarded-Proto $scheme;\nproxy_set_header Upgrade $http_upgrade;\nproxy_set_header Connection $connection_upgrade;\nproxy_read_timeout 300s;\n}\n")
}

func writeAccess(out *strings.Builder, p firewall.Policy, port int) {
	if !p.Enabled {
		return
	}
	for _, r := range p.Rules {
		if r.Port != 0 && r.Port != port {
			continue
		}
		fmt.Fprintf(out, "%s %s;\n", r.Action, r.Source)
	}
	if p.Default == "deny" {
		out.WriteString("deny all;\n")
	} else {
		out.WriteString("allow all;\n")
	}
}
