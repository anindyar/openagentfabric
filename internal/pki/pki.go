// Package pki issues the cluster's mTLS certificates: one CA per cluster,
// generated at install time, a server certificate per node agent and a
// client certificate for oafd. Nothing is ever committed; keys stay on the
// nodes (CLAUDE.md "Never commit secrets").
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ControlPlaneCN is the common name of the only client a node agent accepts.
const ControlPlaneCN = "oafd"

// Validity periods.
const (
	CAValidity   = 10 * 365 * 24 * time.Hour
	LeafValidity = 2 * 365 * 24 * time.Hour
)

// Pair is a PEM certificate and its PEM private key.
type Pair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA creates a self-signed cluster CA.
func NewCA(cluster string) (Pair, error) {
	tmpl := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "oaf cluster CA " + cluster, Organization: []string{"openagentfabric"}},
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	return create(tmpl, nil, nil, CAValidity)
}

// LeafOptions describe a certificate to issue.
type LeafOptions struct {
	CommonName string
	DNSNames   []string
	IPs        []net.IP
	Server     bool
	Client     bool
}

// Issue signs a leaf certificate with the CA.
func Issue(ca Pair, o LeafOptions) (Pair, error) {
	caCert, caKey, err := parse(ca)
	if err != nil {
		return Pair{}, fmt.Errorf("CA: %w", err)
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: o.CommonName, Organization: []string{"openagentfabric"}},
		DNSNames:    o.DNSNames,
		IPAddresses: o.IPs,
		KeyUsage:    x509.KeyUsageDigitalSignature,
	}
	if o.Server {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if o.Client {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	}
	return create(tmpl, caCert, caKey, LeafValidity)
}

func create(tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, validity time.Duration) (Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return Pair{}, err
	}
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-5 * time.Minute) // tolerate small clock skew
	tmpl.NotAfter = time.Now().Add(validity)
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		return Pair{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Pair{}, err
	}
	return Pair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func parse(p Pair) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(p.CertPEM)
	kb, _ := pem.Decode(p.KeyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("invalid PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func pool(caPEM []byte) (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no CA certificate in PEM")
	}
	return p, nil
}

// ServerTLS is the node agent's TLS config: it requires a client certificate
// from the cluster CA whose common name is allowedClientCN.
func ServerTLS(caPEM []byte, node Pair, allowedClientCN string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(node.CertPEM, node.KeyPEM)
	if err != nil {
		return nil, err
	}
	cas, err := pool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    cas,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || cs.PeerCertificates[0].Subject.CommonName != allowedClientCN {
				return fmt.Errorf("client certificate is not %q", allowedClientCN)
			}
			return nil
		},
	}, nil
}

// ClientTLS is oafd's TLS config for talking to the node named serverName.
func ClientTLS(caPEM []byte, client Pair, serverName string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(client.CertPEM, client.KeyPEM)
	if err != nil {
		return nil, err
	}
	cas, err := pool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      cas,
		ServerName:   serverName,
	}, nil
}

// File names inside a PKI directory.
const (
	CACertFile = "ca.crt"
	CAKeyFile  = "ca.key"
)

// Save writes a pair as <dir>/<name>.crt and <dir>/<name>.key (key mode 0600).
func Save(dir, name string, p Pair) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".crt"), p.CertPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".key"), p.KeyPEM, 0o600)
}

// Load reads <dir>/<name>.crt and <dir>/<name>.key.
func Load(dir, name string) (Pair, error) {
	c, err := os.ReadFile(filepath.Join(dir, name+".crt"))
	if err != nil {
		return Pair{}, err
	}
	k, err := os.ReadFile(filepath.Join(dir, name+".key"))
	if err != nil {
		return Pair{}, err
	}
	return Pair{CertPEM: c, KeyPEM: k}, nil
}
