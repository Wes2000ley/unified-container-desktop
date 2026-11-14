package pki

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestRoleCertificatesAndStableTrustAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"client", "server"} {
		pair, err := tls.LoadX509KeyPair(filepath.Join(dir, role+".pem"), filepath.Join(dir, role+"-key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if role == "server" {
			if err = cert.VerifyHostname("localhost"); err != nil {
				t.Fatal(err)
			}
		} else if cert.Subject.CommonName != "ucd-controller" {
			t.Fatal("wrong client identity")
		}
	}
	if err = Ensure(dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if string(before) != string(after) {
		t.Fatal("restart replaced machine trust")
	}
	if err = os.WriteFile(filepath.Join(dir, "client-key.pem"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Ensure(dir); err == nil {
		t.Fatal("broken private key was silently replaced")
	}
}
