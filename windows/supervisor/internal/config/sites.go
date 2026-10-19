package config

import (
	"net/url"
	"strings"
)

// SiteHosts returns the host names of every configured site, without ports.
func (c Config) SiteHosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range c.Web.Sites {
		for _, a := range s.Addresses {
			if h := SiteURL(a, s.TLS).Hostname(); h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// CLIURL is overwrite.cli.url, the base of links Nextcloud generates outside
// a request (mails, notifications, background jobs): the public site if
// there is one, else the first address other clients can reach (not
// localhost), else the first address.
func (c Config) CLIURL() string {
	var first, lan string
	for _, s := range c.Web.Sites {
		for _, a := range s.Addresses {
			u := SiteURL(a, s.TLS)
			full := strings.TrimRight(u.String(), "/")
			switch {
			case s.TLS == "acme":
				return full
			case first == "":
				first = full
			}
			if lan == "" && !isLoopback(u.Hostname()) {
				lan = full
			}
		}
	}
	if lan != "" {
		return lan
	}
	return first
}

// SiteURL parses a site address ("host", "host:port" or a URL).
// Invalid addresses return an empty URL.
func SiteURL(addr, tls string) *url.URL {
	if !strings.Contains(addr, "://") {
		scheme := "https://"
		if tls == "off" {
			scheme = "http://"
		}
		addr = scheme + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return &url.URL{}
	}
	return u
}

func isLoopback(h string) bool {
	return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}
