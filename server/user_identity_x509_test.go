// Copyright 2018-2020 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
)

// generateTestClientCert builds a self-signed RSA client certificate
// for the test cases. The validity window can be shifted via notBefore
// / notAfter to exercise the expiry path.
func generateTestClientCert(t *testing.T, cn string, notBefore, notAfter time.Time) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"Cybus"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return der, key
}

// signWithRSA produces an RSA-SHA256 PKCS1v15 signature suitable for
// the Basic256Sha256 UserTokenSignature algorithm.
func signWithRSA(t *testing.T, key *rsa.PrivateKey, data []byte) []byte {
	t.Helper()
	h := sha256.Sum256(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		t.Fatalf("rsa.SignPKCS1v15: %v", err)
	}
	return sig
}

func TestValidateX509IdentityToken_HappyPath(t *testing.T) {
	now := time.Now()
	clientDER, clientKey := generateTestClientCert(t, "test-user", now.Add(-time.Hour), now.Add(time.Hour))
	serverCert := []byte("server-cert-bytes")
	serverNonce := []byte("01234567890123456789012345678901")

	cfg := &serverConfig{trustedClientCerts: [][]byte{clientDER}}
	tok := &ua.X509IdentityToken{PolicyID: "x509", CertificateData: clientDER}
	signed := append([]byte{}, serverCert...)
	signed = append(signed, serverNonce...)
	sig := &ua.SignatureData{
		Algorithm: "http://www.w3.org/2000/09/xmldsig#rsa-sha256",
		Signature: signWithRSA(t, clientKey, signed),
	}

	cert, identity, err := ValidateX509IdentityToken(cfg, tok, sig, serverCert, serverNonce)
	if err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil parsed certificate")
	}
	if identity != "test-user" {
		t.Fatalf("identity = %q, want %q", identity, "test-user")
	}
}

func TestValidateX509IdentityToken_UntrustedCert(t *testing.T) {
	now := time.Now()
	clientDER, clientKey := generateTestClientCert(t, "rogue-user", now.Add(-time.Hour), now.Add(time.Hour))
	otherDER, _ := generateTestClientCert(t, "other-user", now.Add(-time.Hour), now.Add(time.Hour))
	serverCert := []byte("server")
	serverNonce := []byte("01234567890123456789012345678901")

	cfg := &serverConfig{trustedClientCerts: [][]byte{otherDER}} // trust an unrelated cert
	tok := &ua.X509IdentityToken{CertificateData: clientDER}
	signed := append([]byte{}, serverCert...)
	signed = append(signed, serverNonce...)
	sig := &ua.SignatureData{
		Algorithm: "http://www.w3.org/2000/09/xmldsig#rsa-sha256",
		Signature: signWithRSA(t, clientKey, signed),
	}

	_, _, err := ValidateX509IdentityToken(cfg, tok, sig, serverCert, serverNonce)
	var ve *x509TokenValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected x509TokenValidationError, got %v", err)
	}
	if ve.Status != ua.StatusBadIdentityTokenRejected {
		t.Fatalf("status = %v, want StatusBadIdentityTokenRejected", ve.Status)
	}
}

func TestValidateX509IdentityToken_ExpiredCert(t *testing.T) {
	now := time.Now()
	expiredDER, expiredKey := generateTestClientCert(t, "expired-user",
		now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	serverCert := []byte("server")
	serverNonce := []byte("01234567890123456789012345678901")

	cfg := &serverConfig{trustedClientCerts: [][]byte{expiredDER}}
	tok := &ua.X509IdentityToken{CertificateData: expiredDER}
	signed := append([]byte{}, serverCert...)
	signed = append(signed, serverNonce...)
	sig := &ua.SignatureData{
		Algorithm: "http://www.w3.org/2000/09/xmldsig#rsa-sha256",
		Signature: signWithRSA(t, expiredKey, signed),
	}

	_, _, err := ValidateX509IdentityToken(cfg, tok, sig, serverCert, serverNonce)
	var ve *x509TokenValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected x509TokenValidationError, got %v", err)
	}
	if ve.Status != ua.StatusBadCertificateInvalid {
		t.Fatalf("status = %v, want StatusBadCertificateInvalid", ve.Status)
	}
}

func TestValidateX509IdentityToken_TamperedSignature(t *testing.T) {
	now := time.Now()
	clientDER, clientKey := generateTestClientCert(t, "test-user", now.Add(-time.Hour), now.Add(time.Hour))
	serverCert := []byte("server")
	serverNonce := []byte("01234567890123456789012345678901")

	cfg := &serverConfig{trustedClientCerts: [][]byte{clientDER}}
	tok := &ua.X509IdentityToken{CertificateData: clientDER}
	signed := append([]byte{}, serverCert...)
	signed = append(signed, serverNonce...)
	good := signWithRSA(t, clientKey, signed)
	tampered := append([]byte(nil), good...)
	// Flip the last byte to corrupt the signature.
	tampered[len(tampered)-1] ^= 0xFF
	sig := &ua.SignatureData{
		Algorithm: "http://www.w3.org/2000/09/xmldsig#rsa-sha256",
		Signature: tampered,
	}

	_, _, err := ValidateX509IdentityToken(cfg, tok, sig, serverCert, serverNonce)
	var ve *x509TokenValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected x509TokenValidationError, got %v", err)
	}
	if ve.Status != ua.StatusBadIdentityTokenRejected {
		t.Fatalf("status = %v, want StatusBadIdentityTokenRejected (got %v)", ve.Status, ve.Status)
	}
}

func TestValidateX509IdentityToken_EmptyTrustList(t *testing.T) {
	now := time.Now()
	clientDER, clientKey := generateTestClientCert(t, "test-user", now.Add(-time.Hour), now.Add(time.Hour))
	serverCert := []byte("server")
	serverNonce := []byte("01234567890123456789012345678901")

	cfg := &serverConfig{} // empty trust list
	tok := &ua.X509IdentityToken{CertificateData: clientDER}
	signed := append([]byte{}, serverCert...)
	signed = append(signed, serverNonce...)
	sig := &ua.SignatureData{
		Algorithm: "http://www.w3.org/2000/09/xmldsig#rsa-sha256",
		Signature: signWithRSA(t, clientKey, signed),
	}

	_, _, err := ValidateX509IdentityToken(cfg, tok, sig, serverCert, serverNonce)
	var ve *x509TokenValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected x509TokenValidationError, got %v", err)
	}
	if ve.Status != ua.StatusBadIdentityTokenRejected {
		t.Fatalf("status = %v, want StatusBadIdentityTokenRejected", ve.Status)
	}
}

func TestValidateX509IdentityToken_CustomIdentityResolver(t *testing.T) {
	now := time.Now()
	clientDER, clientKey := generateTestClientCert(t, "irrelevant-cn", now.Add(-time.Hour), now.Add(time.Hour))
	serverCert := []byte("server")
	serverNonce := []byte("01234567890123456789012345678901")

	cfg := &serverConfig{
		trustedClientCerts: [][]byte{clientDER},
		userIdentityFromCert: func(c *x509.Certificate) (string, error) {
			// Custom hook: derive identity from serial number.
			return "serial:" + c.SerialNumber.String(), nil
		},
	}
	tok := &ua.X509IdentityToken{CertificateData: clientDER}
	signed := append([]byte{}, serverCert...)
	signed = append(signed, serverNonce...)
	sig := &ua.SignatureData{
		Algorithm: "http://www.w3.org/2000/09/xmldsig#rsa-sha256",
		Signature: signWithRSA(t, clientKey, signed),
	}

	_, identity, err := ValidateX509IdentityToken(cfg, tok, sig, serverCert, serverNonce)
	if err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	if len(identity) == 0 || identity[:7] != "serial:" {
		t.Fatalf("identity = %q, want serial:* prefix", identity)
	}
}

func TestValidateX509IdentityToken_IdentityResolverError(t *testing.T) {
	now := time.Now()
	clientDER, clientKey := generateTestClientCert(t, "test-user", now.Add(-time.Hour), now.Add(time.Hour))
	serverCert := []byte("server")
	serverNonce := []byte("01234567890123456789012345678901")

	cfg := &serverConfig{
		trustedClientCerts: [][]byte{clientDER},
		userIdentityFromCert: func(c *x509.Certificate) (string, error) {
			return "", errors.New("not mappable")
		},
	}
	tok := &ua.X509IdentityToken{CertificateData: clientDER}
	signed := append([]byte{}, serverCert...)
	signed = append(signed, serverNonce...)
	sig := &ua.SignatureData{
		Algorithm: "http://www.w3.org/2000/09/xmldsig#rsa-sha256",
		Signature: signWithRSA(t, clientKey, signed),
	}

	_, _, err := ValidateX509IdentityToken(cfg, tok, sig, serverCert, serverNonce)
	var ve *x509TokenValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected x509TokenValidationError, got %v", err)
	}
	if ve.Status != ua.StatusBadIdentityTokenRejected {
		t.Fatalf("status = %v, want StatusBadIdentityTokenRejected", ve.Status)
	}
}

func TestTrustedClientCertOption(t *testing.T) {
	der1 := []byte{0x01, 0x02, 0x03}
	der2 := []byte{0x04, 0x05, 0x06}
	srv := New(TrustedClientCert(der1), TrustedClientCert(der2))
	if len(srv.cfg.trustedClientCerts) != 2 {
		t.Fatalf("expected 2 trusted certs, got %d", len(srv.cfg.trustedClientCerts))
	}
	// Empty input must be silently ignored.
	srv2 := New(TrustedClientCert(nil), TrustedClientCert([]byte{}))
	if len(srv2.cfg.trustedClientCerts) != 0 {
		t.Fatalf("expected 0 trusted certs after empty inputs, got %d", len(srv2.cfg.trustedClientCerts))
	}
}

func TestUserIdentityFromCertOption(t *testing.T) {
	srv := New(UserIdentityFromCert(func(*x509.Certificate) (string, error) {
		return "fixed", nil
	}))
	if srv.cfg.userIdentityFromCert == nil {
		t.Fatal("expected userIdentityFromCert to be set")
	}
	got, err := srv.cfg.userIdentityFromCert(&x509.Certificate{})
	if err != nil || got != "fixed" {
		t.Fatalf("hook returned (%q, %v); want (%q, nil)", got, err, "fixed")
	}
}

func TestSessionUserIdentityDefaultEmpty(t *testing.T) {
	sb := newSessionBroker(nil)
	s := sb.NewSession()
	if got := s.UserIdentity(); got != "" {
		t.Fatalf("default UserIdentity = %q, want empty", got)
	}
	// Nil-receiver guard.
	var nilSess *session
	if got := nilSess.UserIdentity(); got != "" {
		t.Fatalf("nil-receiver UserIdentity = %q, want empty", got)
	}
}
