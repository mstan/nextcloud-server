// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

// EnsureCertificates maintains the LAN certificate, preserving an existing
// Caddy root on migration so clients keep their existing trust anchor.
// It reports whether Apache needs to reload its certificate.
func EnsureCertificates(c config.Config) (bool, error) {
	hosts, err := virtualHosts(c)
	if err != nil {
		return false, err
	}
	var names []string
	for _, h := range hosts {
		if h.TLS == "internal" {
			names = append(names, h.Host)
		}
	}
	if len(names) == 0 {
		return false, nil
	}
	dir := filepath.FromSlash(c.TLSDir())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	rootKey := filepath.Join(dir, "root.key")
	if _, err := os.Stat(c.CARootPath()); os.IsNotExist(err) {
		legacy := filepath.Join(c.Paths.StateDir, "caddy", "pki", "authorities", "local")
		if _, err := os.Stat(filepath.Join(legacy, "root.crt")); err == nil {
			for _, name := range []string{"root.key", "root.crt"} {
				b, err := os.ReadFile(filepath.Join(legacy, name))
				if err != nil {
					return false, fmt.Errorf("migrate local CA: %w", err)
				}
				if err := writeMode(filepath.Join(dir, name), b, 0o600); err != nil {
					return false, err
				}
			}
		} else if !os.IsNotExist(err) {
			return false, err
		} else {
			if _, err := os.Stat(rootKey); !os.IsNotExist(err) {
				return false, fmt.Errorf("local CA key exists without its certificate: %s", rootKey)
			}
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return false, err
			}
			serial, err := certificateSerial()
			if err != nil {
				return false, err
			}
			now := time.Now()
			ca := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "ncwin Local CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
			if err := writeCertificate(c.CARootPath(), rootKey, ca, ca, key, key); err != nil {
				return false, err
			}
		}
	} else if err != nil {
		return false, err
	}
	pair, err := tls.LoadX509KeyPair(c.CARootPath(), rootKey)
	if err != nil {
		return false, fmt.Errorf("load local CA: %w", err)
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false, err
	}
	now := time.Now()
	if !ca.IsCA || ca.NotBefore.After(now) || ca.NotAfter.Before(now.Add(31*24*time.Hour)) {
		return false, fmt.Errorf("local CA is invalid or expires within 31 days: %s", c.CARootPath())
	}
	if leaf, err := tls.LoadX509KeyPair(serverCert(c), serverKey(c)); err == nil {
		cert, err := x509.ParseCertificate(leaf.Certificate[0])
		if err == nil && cert.NotBefore.Before(now) && cert.NotAfter.After(now.Add(30*24*time.Hour)) && cert.CheckSignatureFrom(ca) == nil {
			valid := true
			for _, h := range names {
				if cert.VerifyHostname(h) != nil {
					valid = false
				}
			}
			if valid {
				return false, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	serial, err := certificateSerial()
	if err != nil {
		return false, err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ca.NotAfter.Before(cert.NotAfter) {
		cert.NotAfter = ca.NotAfter
	}
	for _, h := range names {
		if ip := net.ParseIP(h); ip != nil {
			cert.IPAddresses = append(cert.IPAddresses, ip)
		} else {
			cert.DNSNames = append(cert.DNSNames, h)
		}
	}
	err = writeCertificate(serverCert(c), serverKey(c), cert, ca, key, pair.PrivateKey.(crypto.Signer))
	return err == nil, err
}

func certificateSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func writeCertificate(certPath, keyPath string, cert, ca *x509.Certificate, key, signer crypto.Signer) error {
	der, err := x509.CreateCertificate(rand.Reader, cert, ca, key.Public(), signer)
	if err != nil {
		return err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeMode(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0o600); err != nil {
		return err
	}
	return writeMode(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}
