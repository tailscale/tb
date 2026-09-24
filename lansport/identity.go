// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package lansport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"time"
)

// Pin identifies a TLS certificate: the SHA-256 of its DER encoding. Since an
// [Identity] is generated fresh on every process start, a pin identifies one
// run of one process.
type Pin [sha256.Size]byte

// PinOf returns the pin of cert.
func PinOf(cert *x509.Certificate) Pin { return sha256.Sum256(cert.Raw) }

// ParsePin parses the hex form of a pin, as produced by [Pin.String].
func ParsePin(s string) (Pin, error) {
	var p Pin
	b, err := hex.DecodeString(s)
	if err != nil {
		return p, err
	}
	if len(b) != len(p) {
		return p, errors.New("lansport: pin has wrong length")
	}
	copy(p[:], b)
	return p, nil
}

// String returns the pin as 64 lowercase hex characters.
func (p Pin) String() string { return hex.EncodeToString(p[:]) }

// Short returns the first 16 hex characters of the pin, for display.
func (p Pin) Short() string { return p.String()[:16] }

// Identity is a process's TLS identity: a self-signed certificate over a
// freshly generated Ed25519 key, used both as the server certificate it
// presents to peers and as the client certificate on connections it makes
// to them. It lives only in memory.
type Identity struct {
	cert tls.Certificate
	pin  Pin
}

// NewIdentity generates an identity whose certificate names name; the name
// is informational, since peers verify by pin.
func NewIdentity(name string) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		// Peers verify by pin, not by name or time, so the validity window
		// only has to satisfy the TLS libraries on both ends. Backdate a
		// little for clock skew.
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Identity{
		cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf},
		pin:  PinOf(leaf),
	}, nil
}

// Pin returns the pin of the identity's certificate.
func (id *Identity) Pin() Pin { return id.pin }

// Certificate returns the identity's TLS certificate.
func (id *Identity) Certificate() tls.Certificate { return id.cert }

// ServerTLSConfig returns a TLS configuration for a server presenting id.
// Any client certificate is accepted at the handshake, so that a peer which
// hasn't verified this process yet can still connect to do so; the request
// handler decides what the presented certificate's pin (see [RequestPin])
// is allowed to do.
func (id *Identity) ServerTLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{id.cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

// ErrPinMismatch is returned by a pinned client TLS configuration when the
// server presented a certificate other than the pinned one, which is what a
// peer restart looks like.
var ErrPinMismatch = errors.New("lansport: peer certificate does not match its pin")

// ClientTLSConfig returns a TLS configuration for connecting to a peer while
// presenting id. If pin is nil, any server certificate is accepted; use that
// for the one connection made over the tailnet to learn a peer's pin, where
// WireGuard has already authenticated the endpoint (see [ResponsePin]).
// Otherwise the server certificate must match the pin exactly.
func (id *Identity) ClientTLSConfig(pin *Pin) *tls.Config {
	cfg := &tls.Config{
		Certificates: []tls.Certificate{id.cert},
		MinVersion:   tls.VersionTLS13,
		// Verification is by pin, not by CA or name; see VerifyConnection.
		InsecureSkipVerify: true,
	}
	if pin != nil {
		want := *pin
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || PinOf(cs.PeerCertificates[0]) != want {
				return ErrPinMismatch
			}
			return nil
		}
	}
	return cfg
}

// RequestPin returns the pin of the client certificate r was made with, if
// any.
func RequestPin(r *http.Request) (Pin, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Pin{}, false
	}
	return PinOf(r.TLS.PeerCertificates[0]), true
}

// ResponsePin returns the pin of the server certificate res was received
// over, if any.
func ResponsePin(res *http.Response) (Pin, bool) {
	if res.TLS == nil || len(res.TLS.PeerCertificates) == 0 {
		return Pin{}, false
	}
	return PinOf(res.TLS.PeerCertificates[0]), true
}
