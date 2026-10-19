// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

func tlsConfig(t *testing.T) config.Config {
	t.Helper()
	c := testConfig(t)
	c.Paths.StateDir = t.TempDir()
	c.Web.Sites = []config.Site{{Addresses: []string{"localhost", "https://192.168.1.5:8443"}, TLS: "internal"}}
	return c
}

func TestCertificatesPersistAndRenew(t *testing.T) {
	c := tlsConfig(t)
	if changed, err := EnsureCertificates(c); !changed || err != nil {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	root, _ := os.ReadFile(c.CARootPath())
	leaf, err := tls.LoadX509KeyPair(serverCert(c), serverKey(c))
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(leaf.Certificate[0])
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(root)
	for _, host := range []string{"localhost", "192.168.1.5"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := EnsureCertificates(c); changed || err != nil {
		t.Fatalf("reissued valid certificate: %v %v", changed, err)
	}
	c.Web.Sites[0].Addresses = append(c.Web.Sites[0].Addresses, "new.lan")
	if changed, err := EnsureCertificates(c); !changed || err != nil {
		t.Fatalf("failed to cover new host: %v %v", changed, err)
	}
	caPair, _ := tls.LoadX509KeyPair(c.CARootPath(), filepath.Join(c.TLSDir(), "root.key"))
	ca, _ := x509.ParseCertificate(caPair.Certificate[0])
	cert.NotAfter = time.Now().Add(24 * time.Hour)
	if err := writeCertificate(serverCert(c), serverKey(c), cert, ca, leaf.PrivateKey.(crypto.Signer), caPair.PrivateKey.(crypto.Signer)); err != nil {
		t.Fatal(err)
	}
	if changed, err := EnsureCertificates(c); !changed || err != nil {
		t.Fatalf("failed to renew: %v %v", changed, err)
	}
	rootAfter, _ := os.ReadFile(c.CARootPath())
	if !bytes.Equal(root, rootAfter) {
		t.Fatal("renewal replaced the trust anchor")
	}
}

func TestCertificatesMigrateLegacyCA(t *testing.T) {
	old := tlsConfig(t)
	if _, err := EnsureCertificates(old); err != nil {
		t.Fatal(err)
	}
	c := tlsConfig(t)
	legacy := filepath.Join(c.Paths.StateDir, "caddy", "pki", "authorities", "local")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root.crt", "root.key"} {
		b, _ := os.ReadFile(filepath.Join(old.TLSDir(), name))
		if err := os.WriteFile(filepath.Join(legacy, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := EnsureCertificates(c); err != nil {
		t.Fatal(err)
	}
	oldRoot, _ := os.ReadFile(old.CARootPath())
	newRoot, _ := os.ReadFile(c.CARootPath())
	if !bytes.Equal(oldRoot, newRoot) {
		t.Fatal("migration replaced the trusted root")
	}
	if err := os.Remove(filepath.Join(c.TLSDir(), "root.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCertificates(c); err == nil {
		t.Fatal("silently replaced CA with missing key")
	}
}

func TestHTTPDoesNotCreateCA(t *testing.T) {
	c := tlsConfig(t)
	c.Web.Sites = []config.Site{{Addresses: []string{"http://localhost"}, TLS: "off"}}
	if changed, err := EnsureCertificates(c); changed || err != nil {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(c.CARootPath()); !os.IsNotExist(err) {
		t.Fatal("HTTP created a CA")
	}
}

func TestWriteAll(t *testing.T) {
	c := tlsConfig(t)
	ini, apache, err := WriteAll(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{ini, apache, filepath.Join(c.NCConfigDir(), NextcloudConfigName)} {
		if b, err := os.ReadFile(path); err != nil || len(b) == 0 {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if _, _, err := WriteAll(c); err != nil {
		t.Fatal(err)
	}
}
