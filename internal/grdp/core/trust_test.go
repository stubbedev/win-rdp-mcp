package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

const testServerName = "rdp.test"

func selfSignedCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: testServerName},
		DNSNames:     []string{testServerName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func startTLSAgainst(t *testing.T, cert tls.Certificate, trust ServerTrust) error {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		_ = tls.Server(serverConn, &tls.Config{Certificates: []tls.Certificate{cert}}).Handshake()
	}()
	err := NewSocketLayer(clientConn, testServerName, trust).StartTLS()
	clientConn.Close()
	<-done
	return err
}

func TestStartTLSRejectsAnUntrustedCertificateByDefault(t *testing.T) {
	cert := selfSignedCertificate(t)
	for _, tc := range []struct {
		name  string
		trust ServerTrust
	}{{"zero value", ServerTrust{}}, {"system roots", TrustSystemRoots()}} {
		err := startTLSAgainst(t, cert, tc.trust)
		var untrusted *UntrustedCertificateError
		if !errors.As(err, &untrusted) {
			t.Fatalf("%s: err = %v, want an UntrustedCertificateError", tc.name, err)
		}
		if want := CertificateSHA256(sha256.Sum256(cert.Certificate[0])); untrusted.SHA256 != want {
			t.Errorf("%s: reported SHA-256 %s, want %s", tc.name, untrusted.SHA256, want)
		}
	}
}

func TestStartTLSAcceptsThePinnedCertificateOnly(t *testing.T) {
	cert := selfSignedCertificate(t)
	pin := CertificateSHA256(sha256.Sum256(cert.Certificate[0]))
	if err := startTLSAgainst(t, cert, TrustPinnedCertificate(pin)); err != nil {
		t.Fatalf("pinned certificate rejected: %v", err)
	}

	other := pin
	other[0] ^= 0xff
	err := startTLSAgainst(t, cert, TrustPinnedCertificate(other))
	var untrusted *UntrustedCertificateError
	if !errors.As(err, &untrusted) || untrusted.SHA256 != pin {
		t.Fatalf("err = %v, want an UntrustedCertificateError naming %s", err, pin)
	}
}

func TestStartTLSAcceptsAnyCertificateOnlyWhenAskedTo(t *testing.T) {
	if err := startTLSAgainst(t, selfSignedCertificate(t), TrustAnyCertificate()); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestParseCertificateSHA256(t *testing.T) {
	want := sha256.Sum256([]byte("rdp"))
	hexDigits := CertificateSHA256(want).String()
	var colons []string
	for i := 0; i < len(hexDigits); i += 2 {
		colons = append(colons, strings.ToUpper(hexDigits[i:i+2]))
	}
	for _, in := range []string{hexDigits, strings.Join(colons, ":"), " " + hexDigits + "\n"} {
		got, err := ParseCertificateSHA256(in)
		if err != nil || got != want {
			t.Errorf("ParseCertificateSHA256(%q) = %s, %v", in, got, err)
		}
	}
	for _, in := range []string{"", hexDigits[:62], hexDigits + "00", "zz" + hexDigits[2:]} {
		if _, err := ParseCertificateSHA256(in); err == nil {
			t.Errorf("ParseCertificateSHA256(%q) accepted", in)
		}
	}
}
