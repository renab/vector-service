package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strings"
	"time"
)

// TLSMaterial is the parsed TLS client material for one identity: the CA
// bundle, the client certificate, and the matching client key. It is
// produced by Load from the configured paths and consumed by internal/db
// to build the connection's *tls.Config.
//
// The material is never serialized, logged, or included in errors: at
// most, byte counts appear.
type TLSMaterial struct {
	// CAFile, CertFile, and KeyFile are the validated paths.
	CAFile   string
	CertFile string
	KeyFile  string

	rootCAs    *x509.CertPool
	cert       tls.Certificate
	leaf       *x509.Certificate
	serverName string
}

// RootCAs returns the pool of trusted CA certificates.
func (m *TLSMaterial) RootCAs() *x509.CertPool { return m.rootCAs }

// ServerName returns the hostname used for server-certificate validation.
func (m *TLSMaterial) ServerName() string { return m.serverName }

// GetClientCertificate returns the client certificate/key pair for the
// TLS handshake.
func (m *TLSMaterial) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return &m.cert, nil
}

// TLSConfig builds the *tls.Config for connections using this material:
// full server verification against RootCAs plus client-certificate
// authentication. InsecureSkipVerify is always false.
func (m *TLSMaterial) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:           tls.VersionTLS12,
		RootCAs:              m.rootCAs,
		GetClientCertificate: m.GetClientCertificate,
		ServerName:           m.serverName,
	}
}

// pairIdentity is the non-secret identity of a parsed client leaf: the DER
// of the certificate and a digest of the public key. No private key
// material is copied into it.
func pairIdentity(leaf *x509.Certificate) []byte {
	var pubDER []byte
	if pk, err := x509.MarshalPKIXPublicKey(leaf.PublicKey); err == nil {
		pubDER = pk
	}
	sum := sha256.Sum256(pubDER)
	out := make([]byte, 0, len(leaf.Raw)+len(sum))
	out = append(out, leaf.Raw...)
	out = append(out, sum[:]...)
	return out
}

// sameClientCertificate reports whether two parsed materials present the
// same client certificate and key pair.
func sameClientCertificate(a, b *TLSMaterial) bool {
	return string(pairIdentity(a.leaf)) == string(pairIdentity(b.leaf))
}

// parseTLSMaterialFromPEM reads TLS material from PEM-encoded bytes.
// caPEM is the CA bundle, certPEM is the client certificate,
// keyPEM is the client private key. serverName is used for server-
// certificate verification. It returns the parsed material with empty
// file paths (those are filled in by the caller when loading from disk).
func parseTLSMaterialFromPEM(
	caPEM, certPEM, keyPEM []byte,
	serverName string,
) (*TLSMaterial, error) {
	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA certificate data contains no valid certificates")
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("client certificate and key do not form a valid pair (%d certificate bytes, %d key bytes)", len(certPEM), len(keyPEM))
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("client certificate data contains no certificates")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, errors.New("client certificate leaf is not a valid X.509 certificate")
	}

	return &TLSMaterial{
		rootCAs:    rootCAs,
		cert:       cert,
		leaf:       leaf,
		serverName: serverName,
	}, nil
}

// NewTLSMaterialFromPEM creates TLSMaterial from in-memory PEM-encoded
// certificate data. caPEM is the CA bundle, certPEM is the client certificate,
// keyPEM is the client private key. serverName is used for server-certificate
// verification. This is intended for testing.
func NewTLSMaterialFromPEM(caPEM, certPEM, keyPEM []byte, serverName string) (*TLSMaterial, error) {
	return parseTLSMaterialFromPEM(caPEM, certPEM, keyPEM, serverName)
}

// parseTLSMaterial reads and validates the CA bundle, client certificate,
// and client key at their paths, and returns the parsed material. Any
// failure is a *Error naming the variable that owns the path.
func parseTLSMaterial(
	caVar, caPath,
	certVar, certPath,
	keyVar, keyPath,
	serverName string,
) (*TLSMaterial, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fail(caVar, fmt.Sprintf("cannot read CA certificate file: %s", describeReadError(err)))
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fail(certVar, fmt.Sprintf("cannot read client certificate file: %s", describeReadError(err)))
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fail(keyVar, fmt.Sprintf("cannot read client key file: %s", describeReadError(err)))
	}

	m, err := parseTLSMaterialFromPEM(caPEM, certPEM, keyPEM, serverName)
	if err != nil {
		return nil, fail(caVar, err.Error())
	}
	m.CAFile = caPath
	m.CertFile = certPath
	m.KeyFile = keyPath
	return m, nil
}

// describeReadError renders an os.ReadFile failure without leaking more
// than the operation already does (the path is named separately by the
// caller).
func describeReadError(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "file does not exist"
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission denied"
	}
	return "read error"
}

// parseURL parses a strict postgres:// or postgresql:// URL. The standard
// library parser percent-decodes userinfo and query values, which a
// configuration input must not be allowed to do; this parser keeps the raw
// string and rejects any percent-escape outright.
func parseURL(raw string) (*url.URL, error) {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" {
		return nil, errors.New("must be a URL with a scheme (postgres:// or postgresql://)")
	}
	if strings.ContainsAny(rest, "%") {
		return nil, errors.New("must not contain percent-escapes")
	}
	u := &url.URL{Scheme: scheme}
	if h, p, found := strings.Cut(rest, "/"); found {
		u.Host = h
		u.Path = "/" + p
	} else {
		u.Host = rest
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	return u, nil
}

// --- test certificate generation ---
//
// The package-internal tests need self-contained TLS fixtures. These
// helpers generate a CA and client certificates in memory and write the
// PEM files to a directory. They are used only by tests.

// testCA is a generated certificate authority.
type testCA struct {
	key *ecdsa.PrivateKey
	crt *x509.Certificate
	pem []byte
}

func newTestCA(cn string) (*testCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	crt, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &testCA{key: key, crt: crt, pem: pemEncode("CERTIFICATE", der)}, nil
}

// testLeaf is a generated client certificate signed by a CA.
type testLeaf struct {
	key     *ecdsa.PrivateKey
	certDER []byte
	certPEM []byte
	keyPEM  []byte
}

func newTestLeaf(cn string, ca *testCA) (*testLeaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.crt, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &testLeaf{
		key:     key,
		certDER: der,
		certPEM: pemEncode("CERTIFICATE", der),
		keyPEM:  pemEncode("EC PRIVATE KEY", keyDER),
	}, nil
}

func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

func randomSerial() (*big.Int, error) {
	// 63-bit positive serial.
	limit := new(big.Int).Lsh(big.NewInt(1), 63)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}
