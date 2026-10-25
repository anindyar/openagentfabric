package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/pki"
)

// TrustRegistry writes the CA for that registry only, once, and refuses
// anything that is not a host:port and a certificate.
func TestTrustRegistry(t *testing.T) {
	ca, err := pki.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := &Server{DockerCertsDir: dir}
	ctx := context.Background()
	resp, err := s.TrustRegistry(ctx, &nodev1.TrustRegistryRequest{Host: "192.0.2.11:5050", CaPem: ca.CertPEM})
	if err != nil || !resp.GetChanged() {
		t.Fatalf("first trust: %v %v", resp, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "192.0.2.11:5050", "ca.crt"))
	if string(got) != string(ca.CertPEM) {
		t.Fatal("CA not written")
	}
	if resp, _ := s.TrustRegistry(ctx, &nodev1.TrustRegistryRequest{Host: "192.0.2.11:5050", CaPem: ca.CertPEM}); resp.GetChanged() {
		t.Fatal("unchanged CA rewritten")
	}
	for _, bad := range []*nodev1.TrustRegistryRequest{
		{Host: "../../etc:5050", CaPem: ca.CertPEM},
		{Host: "192.0.2.11", CaPem: ca.CertPEM},
		{Host: "192.0.2.11:5050", CaPem: ca.KeyPEM},
		{Host: "192.0.2.11:5050", CaPem: []byte("hello")},
	} {
		if _, err := s.TrustRegistry(ctx, bad); err == nil {
			t.Fatalf("accepted %q", bad.GetHost())
		}
	}
}
