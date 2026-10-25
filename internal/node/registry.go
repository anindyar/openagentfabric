package node

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"connectrpc.com/connect/v2"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
)

// DockerCertsDir is where docker looks for a registry's CA.
const DockerCertsDir = "/etc/docker/certs.d"

var registryHost = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}:[0-9]{2,5}$`)

// TrustRegistry implements NodeService.
func (s *Server) TrustRegistry(_ context.Context, req *nodev1.TrustRegistryRequest) (*nodev1.TrustRegistryResponse, error) {
	if !registryHost.MatchString(req.GetHost()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "host must be host:port")
	}
	if err := onlyCertificates(req.GetCaPem()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	dir := s.DockerCertsDir
	if dir == "" {
		dir = DockerCertsDir
	}
	path := filepath.Join(dir, req.GetHost(), "ca.crt")
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, req.GetCaPem()) {
		return &nodev1.TrustRegistryResponse{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, req.GetCaPem(), 0o644); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	return &nodev1.TrustRegistryResponse{Changed: true}, nil
}

// onlyCertificates accepts PEM that holds certificates and nothing else.
func onlyCertificates(data []byte) error {
	n := 0
	for rest := bytes.TrimSpace(data); len(rest) > 0; rest = bytes.TrimSpace(rest) {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil || b.Type != "CERTIFICATE" {
			return errors.New("ca_pem must hold PEM certificates only")
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("ca_pem holds no certificate")
	}
	return nil
}
