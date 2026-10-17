package pki

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func mustCA(t *testing.T) Pair {
	t.Helper()
	ca, err := NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func mustIssue(t *testing.T, ca Pair, o LeafOptions) Pair {
	t.Helper()
	p, err := Issue(ca, o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// call serves HTTPS on loopback with the server config and makes one request
// with the client config, the same way oafd talks to oaf-node.
func call(t *testing.T, server, client *tls.Config) error {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = server
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: client}}
	resp, err := hc.Get(srv.URL)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func setup(t *testing.T) (ca, node, oafd Pair) {
	ca = mustCA(t)
	node = mustIssue(t, ca, LeafOptions{CommonName: "spark-1", DNSNames: []string{"spark-1"}, IPs: []net.IP{net.ParseIP("192.168.1.11")}, Server: true})
	oafd = mustIssue(t, ca, LeafOptions{CommonName: ControlPlaneCN, Client: true})
	return
}

func TestMutualTLSAccepted(t *testing.T) {
	ca, node, oafd := setup(t)
	srv, err := ServerTLS(ca.CertPEM, node, ControlPlaneCN)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := ClientTLS(ca.CertPEM, oafd, "spark-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := call(t, srv, cl); err != nil {
		t.Fatalf("mTLS call failed: %v", err)
	}
}

func TestWrongClientCNRejected(t *testing.T) {
	ca, node, _ := setup(t)
	other := mustIssue(t, ca, LeafOptions{CommonName: "spark-2", Client: true})
	srv, _ := ServerTLS(ca.CertPEM, node, ControlPlaneCN)
	cl, _ := ClientTLS(ca.CertPEM, other, "spark-1")
	if err := call(t, srv, cl); err == nil {
		t.Fatal("a non-oafd client must be rejected")
	}
}

func TestForeignCARejected(t *testing.T) {
	ca, node, _ := setup(t)
	foreign := mustCA(t)
	impostor := mustIssue(t, foreign, LeafOptions{CommonName: ControlPlaneCN, Client: true})
	srv, _ := ServerTLS(ca.CertPEM, node, ControlPlaneCN)
	cl, _ := ClientTLS(ca.CertPEM, impostor, "spark-1")
	if err := call(t, srv, cl); err == nil {
		t.Fatal("a client from another CA must be rejected")
	}
}

func TestWrongServerNameRejected(t *testing.T) {
	ca, node, oafd := setup(t)
	srv, _ := ServerTLS(ca.CertPEM, node, ControlPlaneCN)
	cl, _ := ClientTLS(ca.CertPEM, oafd, "spark-2")
	if err := call(t, srv, cl); err == nil {
		t.Fatal("oafd must reject a node whose certificate names another host")
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	ca := mustCA(t)
	if err := Save(dir, "ca", ca); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "ca")
	if err != nil || string(got.CertPEM) != string(ca.CertPEM) || string(got.KeyPEM) != string(ca.KeyPEM) {
		t.Fatalf("round trip failed: %v", err)
	}
}
