// Package pki provisions the private, machine-local helper trust domain.
package pki

import (
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

	"github.com/skyri/ucd/internal/state"
)

// Ensure creates a CA and role-specific certificates, or verifies existing material.
// Directory ACLs are installed before first service startup by the machine installer.
func Ensure(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	caPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	caPEM, err := os.ReadFile(caPath)
	var ca *x509.Certificate
	var key *ecdsa.PrivateKey
	if os.IsNotExist(err) {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		tpl, err := certificate("ucd-machine-ca")
		if err != nil {
			return err
		}
		tpl.IsCA = true
		tpl.BasicConstraintsValid = true
		tpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		der, e := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
		if e != nil {
			return e
		}
		ca, e = x509.ParseCertificate(der)
		if e != nil {
			return e
		}
		caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		if err = writeKey(keyPath, key); err != nil {
			return err
		}
		if err = state.AtomicWrite(caPath, caPEM, 0644); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		block, _ := pem.Decode(caPEM)
		if block == nil {
			return fmt.Errorf("invalid machine CA certificate")
		}
		ca, err = x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		b, e := os.ReadFile(keyPath)
		if e != nil {
			return e
		}
		block, _ = pem.Decode(b)
		if block == nil {
			return fmt.Errorf("invalid machine CA private key")
		}
		raw, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return e
		}
		var ok bool
		key, ok = raw.(*ecdsa.PrivateKey)
		if !ok {
			return fmt.Errorf("machine CA private key must use ECDSA")
		}
		if !key.PublicKey.Equal(ca.PublicKey) {
			return fmt.Errorf("machine CA certificate and private key do not match")
		}
	}
	if !ca.IsCA || time.Now().After(ca.NotAfter) {
		return fmt.Errorf("machine CA is expired or invalid; renew the machine trust domain before starting the helper")
	}
	for _, role := range []string{"client", "server"} {
		certPath := filepath.Join(dir, role+".pem")
		privatePath := filepath.Join(dir, role+"-key.pem")
		if _, err := os.Stat(certPath); err == nil {
			pair, e := tls.LoadX509KeyPair(certPath, privatePath)
			if e != nil {
				return fmt.Errorf("load %s certificate: %w", role, e)
			}
			leaf, e := x509.ParseCertificate(pair.Certificate[0])
			if e != nil {
				return e
			}
			roots := x509.NewCertPool()
			roots.AddCert(ca)
			usage := x509.ExtKeyUsageClientAuth
			if role == "server" {
				usage = x509.ExtKeyUsageServerAuth
			}
			if _, e = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}}); e != nil {
				return fmt.Errorf("verify %s certificate: %w", role, e)
			}
			if role == "server" {
				if e = leaf.VerifyHostname("localhost"); e != nil {
					return e
				}
			} else if leaf.Subject.CommonName != "ucd-controller" {
				return fmt.Errorf("unexpected controller certificate identity")
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		private, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			return e
		}
		tpl, e := certificate("ucd-controller")
		if e != nil {
			return e
		}
		tpl.KeyUsage = x509.KeyUsageDigitalSignature
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		if role == "server" {
			tpl.Subject.CommonName = "ucd-helper"
			tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tpl.DNSNames = []string{"localhost"}
			tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
		}
		der, e := x509.CreateCertificate(rand.Reader, tpl, ca, &private.PublicKey, key)
		if e != nil {
			return e
		}
		if e = writeKey(privatePath, private); e != nil {
			return e
		}
		if e = state.AtomicWrite(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); e != nil {
			return e
		}
	}
	return nil
}
func certificate(name string) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0), BasicConstraintsValid: true}, nil
}
func writeKey(path string, key *ecdsa.PrivateKey) error {
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return state.AtomicWrite(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), 0600)
}
