package core

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

type CertificateSHA256 [sha256.Size]byte

func (c CertificateSHA256) String() string {
	return hex.EncodeToString(c[:])
}

func ParseCertificateSHA256(s string) (CertificateSHA256, error) {
	var sum CertificateSHA256
	digits := strings.ReplaceAll(strings.TrimSpace(s), ":", "")
	if len(digits) != hex.EncodedLen(len(sum)) {
		return sum, fmt.Errorf("certificate SHA-256 %q: want %d hex digits, got %d", s, hex.EncodedLen(len(sum)), len(digits))
	}
	if _, err := hex.Decode(sum[:], []byte(digits)); err != nil {
		return sum, fmt.Errorf("certificate SHA-256 %q: %w", s, err)
	}
	return sum, nil
}

type trustMode uint8

const (
	trustSystemRoots trustMode = iota
	trustPinnedCertificate
	trustAnyCertificate
)

type ServerTrust struct {
	mode trustMode
	pin  CertificateSHA256
}

func TrustSystemRoots() ServerTrust { return ServerTrust{mode: trustSystemRoots} }

func TrustPinnedCertificate(pin CertificateSHA256) ServerTrust {
	return ServerTrust{mode: trustPinnedCertificate, pin: pin}
}

func TrustAnyCertificate() ServerTrust { return ServerTrust{mode: trustAnyCertificate} }

type UntrustedCertificateError struct {
	ServerName string
	SHA256     CertificateSHA256
	Err        error
}

func (e *UntrustedCertificateError) Error() string {
	return fmt.Sprintf("the TLS certificate of RDP server %s (SHA-256 %s) is not trusted: %v", e.ServerName, e.SHA256, e.Err)
}

func (e *UntrustedCertificateError) Unwrap() error { return e.Err }

func (t ServerTrust) clientConfig(serverName string) *tls.Config {
	cfg := &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
	}
	switch t.mode {
	case trustPinnedCertificate:
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error { return t.verifyPinned(serverName, cs) }
	case trustAnyCertificate:
		cfg.InsecureSkipVerify = true
	}
	return cfg
}

func (t ServerTrust) verifyPinned(serverName string, cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return fmt.Errorf("RDP server %s presented no TLS certificate", serverName)
	}
	got := CertificateSHA256(sha256.Sum256(cs.PeerCertificates[0].Raw))
	if got != t.pin {
		return &UntrustedCertificateError{
			ServerName: serverName,
			SHA256:     got,
			Err:        fmt.Errorf("it does not match the pinned SHA-256 %s", t.pin),
		}
	}
	return nil
}

func explainHandshakeError(serverName string, err error) error {
	var untrusted *UntrustedCertificateError
	if errors.As(err, &untrusted) {
		return untrusted
	}
	var verr *tls.CertificateVerificationError
	if errors.As(err, &verr) && len(verr.UnverifiedCertificates) > 0 {
		return &UntrustedCertificateError{
			ServerName: serverName,
			SHA256:     sha256.Sum256(verr.UnverifiedCertificates[0].Raw),
			Err:        verr.Err,
		}
	}
	return fmt.Errorf("TLS handshake with RDP server %s: %w", serverName, err)
}
