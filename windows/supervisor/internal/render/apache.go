// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

type virtualHost struct {
	Host, TLS, Redirect string
	Port                int
}

func (h virtualHost) ServerName() string {
	if strings.Contains(h.Host, ":") {
		return "[" + h.Host + "]"
	}
	return h.Host
}

type apacheConfig struct {
	Config  config.Config
	Hosts   []virtualHost
	Ports   []int
	Managed []string
}

var dnsName = regexp.MustCompile(`^[a-zA-Z0-9_](?:[a-zA-Z0-9_.-]*[a-zA-Z0-9_])?$`)

func virtualHosts(c config.Config) ([]virtualHost, error) {
	var hosts []virtualHost
	for _, s := range c.Web.Sites {
		for _, address := range s.Addresses {
			u := config.SiteURL(address, s.TLS)
			h := u.Hostname()
			if h == "" || (net.ParseIP(h) == nil && !dnsName.MatchString(h)) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				return nil, fmt.Errorf("invalid site address %q", address)
			}
			if (s.TLS == "off" && u.Scheme != "http") || (s.TLS != "off" && u.Scheme != "https") {
				return nil, fmt.Errorf("site address %q conflicts with tls = %q", address, s.TLS)
			}
			port := c.Web.HTTPSPort
			if s.TLS == "off" {
				port = c.Web.HTTPPort
			}
			if u.Port() != "" {
				parsed, err := strconv.Atoi(u.Port())
				port = parsed
				if err != nil || port < 1 || port > 65535 {
					return nil, fmt.Errorf("invalid site port in %q", address)
				}
			}
			redirect := "https://" + h
			if strings.Contains(h, ":") {
				redirect = "https://[" + h + "]"
			}
			if u.Port() != "" && u.Port() != "443" {
				redirect += ":" + u.Port()
			}
			hosts = append(hosts, virtualHost{Host: h, Port: port, TLS: s.TLS, Redirect: redirect})
		}
	}
	return hosts, nil
}

// Apache renders native Windows Apache with the thread-safe PHP module.
func Apache(c config.Config) ([]byte, error) {
	// Apache expands quoted values too; reject configuration syntax in paths.
	for _, value := range []string{c.Paths.InstallDir, c.Paths.StateDir, c.Paths.DataDir, c.ConfigPath(), c.Web.ACMEEmail} {
		if strings.ContainsAny(value, "\r\n\"<>\x00") || strings.Contains(value, "${") {
			return nil, fmt.Errorf("value cannot be represented in Apache configuration: %q", value)
		}
	}
	hosts, err := virtualHosts(c)
	if err != nil {
		return nil, err
	}
	d := apacheConfig{Config: c, Hosts: hosts}
	ports := map[int]string{}
	managed := map[string]bool{}
	for _, h := range hosts {
		protocol := "https"
		if h.TLS == "off" {
			protocol = "http"
		}
		if prior, ok := ports[h.Port]; ok && prior != protocol {
			return nil, fmt.Errorf("port %d mixes HTTP and HTTPS", h.Port)
		}
		if _, ok := ports[h.Port]; !ok {
			d.Ports = append(d.Ports, h.Port)
		}
		ports[h.Port] = protocol
		if h.TLS == "acme" && !managed[h.Host] {
			if net.ParseIP(h.Host) != nil {
				return nil, fmt.Errorf("ACME requires a DNS name: %q", h.Host)
			}
			managed[h.Host] = true
			d.Managed = append(d.Managed, h.Host)
		}
	}
	if _, ok := ports[c.Web.HTTPPort]; !ok {
		d.Ports = append(d.Ports, c.Web.HTTPPort)
	} else if ports[c.Web.HTTPPort] != "http" {
		return nil, fmt.Errorf("web.http_port is also used for HTTPS")
	}
	return exec("httpd.conf.tmpl", d)
}

func serverCert(c config.Config) string {
	return filepath.Join(filepath.FromSlash(c.TLSDir()), "server.crt")
}
func serverKey(c config.Config) string {
	return filepath.Join(filepath.FromSlash(c.TLSDir()), "server.key")
}
