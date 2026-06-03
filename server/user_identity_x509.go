// Copyright 2018-2020 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package server

import (
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uapolicy"
)

// TrustedClientCert appends a DER-encoded X.509 client certificate to
// the server's trust list for X.509 UserIdentityToken validation.
//
// On ActivateSession with a UserTokenTypeCertificate token the server
// performs a chain build against this list (plus expiry / signature
// checks). When the trust list is empty, every X.509 token is rejected
// with StatusBadIdentityTokenRejected.
//
// The certificate is stored verbatim; the option performs no parsing.
// Invalid DER surfaces at ActivateSession time as a chain-build failure.
//
// Pass the cert bytes in DER form. PEM callers can decode with
// pem.Decode + ".Bytes".
func TrustedClientCert(derCert []byte) Option {
	return func(s *serverConfig) {
		if len(derCert) == 0 {
			return
		}
		cp := make([]byte, len(derCert))
		copy(cp, derCert)
		s.trustedClientCerts = append(s.trustedClientCerts, cp)
	}
}

// UserIdentityFromCert installs the function the server uses to map a
// successfully-validated client certificate to a user identity string
// at ActivateSession time. The returned identity is exposed via
// Session.UserIdentity() for the consuming application to use as a
// drop-in for the UserName presented under UserTokenTypeUserName.
//
// The function is invoked only after chain build, expiry, and token-
// signature verification have all succeeded. Returning a non-nil error
// causes ActivateSession to fail with StatusBadIdentityTokenRejected
// (the certificate was structurally valid but is not mappable to a
// known user — the spec-mandated terminal condition).
//
// When this option is not set, the default mapping is the certificate's
// Subject Common Name (CN); an empty CN with no error is treated as a
// rejection.
func UserIdentityFromCert(fn func(*x509.Certificate) (string, error)) Option {
	return func(s *serverConfig) {
		s.userIdentityFromCert = fn
	}
}

// defaultUserIdentityFromCert maps a validated client certificate to
// its Subject Common Name. Returns an empty string + nil if CN is
// empty so the caller surfaces the spec-mandated
// StatusBadIdentityTokenRejected without a more specific reason.
func defaultUserIdentityFromCert(cert *x509.Certificate) (string, error) {
	if cert == nil {
		return "", errors.New("nil certificate")
	}
	return cert.Subject.CommonName, nil
}

// X509TokenValidationError is returned by ValidateX509IdentityToken
// when the token must be rejected. The Status field is the OPC UA
// status code the caller must propagate to the client per Part 4
// §5.6.3.
type X509TokenValidationError struct {
	Status ua.StatusCode
	Reason string
}

func (e *X509TokenValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Status, e.Reason)
}

// X509TokenStatusFromError extracts the spec-mandated StatusCode from a
// validation error returned by ValidateX509IdentityToken. The second
// return is false when err is not (or does not wrap) an
// *X509TokenValidationError, so callers can supply their own fallback
// without leaking a non-spec status code.
func X509TokenStatusFromError(err error) (ua.StatusCode, bool) {
	var ve *X509TokenValidationError
	if errors.As(err, &ve) {
		return ve.Status, true
	}
	return 0, false
}

// ValidateX509IdentityToken validates a client X.509 UserIdentityToken
// against the server's configured trust list and verifies the token
// signature against (server certificate || server nonce) per OPC UA
// Part 4 §5.6.3 and §7.36.4.
//
// On success it returns the parsed client certificate and the resolved
// user identity string produced by the cfg.userIdentityFromCert hook
// (or the default CN-based mapping when no hook is configured).
//
// On failure it returns an *x509TokenValidationError carrying the
// spec-mandated StatusCode. The two terminal codes used are:
//
//   - ua.StatusBadIdentityTokenRejected — for trust / mapping / token
//     signature failures (Part 4 §5.6.3 "the identity token is not
//     accepted by the server").
//   - ua.StatusBadCertificateInvalid — for structural / expiry failures
//     (Part 4 §A.2 certificate validation step).
//
// The function makes no assumption about the channel security policy
// other than reading it from the request's signature algorithm field;
// callers that need to enforce a particular channel mode must do so
// before calling this function.
func ValidateX509IdentityToken(
	srv *Server,
	token *ua.X509IdentityToken,
	tokenSignature *ua.SignatureData,
	serverCertificate []byte,
	serverNonce []byte,
) (*x509.Certificate, string, error) {
	if srv == nil || srv.cfg == nil {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadInternalError,
			Reason: "nil server",
		}
	}
	cfg := srv.cfg
	if token == nil {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenInvalid,
			Reason: "nil X509IdentityToken",
		}
	}
	if len(token.CertificateData) == 0 {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenInvalid,
			Reason: "empty certificate data",
		}
	}

	cert, err := x509.ParseCertificate(token.CertificateData)
	if err != nil {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadCertificateInvalid,
			Reason: "parse client certificate: " + err.Error(),
		}
	}

	// Expiry check (Part 4 §A.2).
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadCertificateInvalid,
			Reason: "certificate not within validity period",
		}
	}

	// Chain build against the configured trust list. The trust list
	// doubles as both root CAs and pre-trusted self-signed leaves; we
	// add every trusted DER to both pools so a self-signed pre-trusted
	// client cert can chain to itself, and a CA-issued client cert can
	// chain via its CA.
	if len(cfg.trustedClientCerts) == 0 {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenRejected,
			Reason: "no trusted client certificates configured",
		}
	}
	roots := x509.NewCertPool()
	intermediates := x509.NewCertPool()
	for _, der := range cfg.trustedClientCerts {
		trusted, perr := x509.ParseCertificate(der)
		if perr != nil {
			continue
		}
		roots.AddCert(trusted)
		intermediates.AddCert(trusted)
	}
	verifyOpts := x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		// No KeyUsages constraint: OPC UA client identity certificates
		// are not required to carry ExtKeyUsageClientAuth (different
		// from TLS). The caller can supply an UserIdentityFromCert hook
		// to enforce stricter checks if needed.
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}
	if _, err := cert.Verify(verifyOpts); err != nil {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenRejected,
			Reason: "untrusted client certificate: " + err.Error(),
		}
	}

	// Token-signature verification: per Part 4 §5.6.3 the client signs
	// (server certificate || server nonce) with its private key. The
	// algorithm URI comes from UserTokenSignature.Algorithm. The signed
	// bytes are the channel's ServerCertificate concatenated with the
	// ServerNonce that was issued at CreateSession time and re-issued at
	// each ActivateSession.
	if tokenSignature == nil {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenRejected,
			Reason: "missing UserTokenSignature",
		}
	}
	rsaPub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadCertificateInvalid,
			Reason: "non-RSA client public key not supported",
		}
	}
	policyURI := signatureURIToPolicyURI(tokenSignature.Algorithm)
	if policyURI == "" {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenRejected,
			Reason: "unsupported UserTokenSignature algorithm: " + tokenSignature.Algorithm,
		}
	}
	algo, err := uapolicy.Asymmetric(policyURI, nil, rsaPub)
	if err != nil {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenRejected,
			Reason: "build verifier: " + err.Error(),
		}
	}
	signed := make([]byte, 0, len(serverCertificate)+len(serverNonce))
	signed = append(signed, serverCertificate...)
	signed = append(signed, serverNonce...)
	if err := algo.VerifySignature(signed, tokenSignature.Signature); err != nil {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenRejected,
			Reason: "token signature verification failed: " + err.Error(),
		}
	}

	// Identity resolution.
	resolver := cfg.userIdentityFromCert
	if resolver == nil {
		resolver = defaultUserIdentityFromCert
	}
	identity, err := resolver(cert)
	if err != nil {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenRejected,
			Reason: "identity resolution: " + err.Error(),
		}
	}
	if identity == "" {
		return nil, "", &X509TokenValidationError{
			Status: ua.StatusBadIdentityTokenRejected,
			Reason: "empty user identity from certificate",
		}
	}
	return cert, identity, nil
}

// signatureURIToPolicyURI maps a UserTokenSignature.Algorithm URI to
// the corresponding SecurityPolicy URI accepted by uapolicy.Asymmetric.
// The mapping covers the asymmetric signature URIs that gopcua's
// uapolicy package implements today; unrecognized URIs return "" so
// callers reject the token rather than silently falling back to a
// permissive verifier.
func signatureURIToPolicyURI(sigURI string) string {
	switch sigURI {
	case "http://www.w3.org/2000/09/xmldsig#rsa-sha1":
		// Basic128Rsa15 / Basic256 both sign with RSA-SHA1.
		return ua.SecurityPolicyURIBasic256
	case "http://www.w3.org/2000/09/xmldsig#rsa-sha256",
		"http://www.w3.org/2001/04/xmldsig-more#rsa-sha256":
		// Basic256Sha256 / Aes128Sha256RsaOaep.
		return ua.SecurityPolicyURIBasic256Sha256
	case "http://opcfoundation.org/UA/security/rsa-pss-sha2-256":
		// Aes256Sha256RsaPss.
		return ua.SecurityPolicyURIAes256Sha256RsaPss
	default:
		return ""
	}
}
