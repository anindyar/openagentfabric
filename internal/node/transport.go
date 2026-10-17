package node

import (
	"crypto/tls"
	"net/http"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"openagentfabric.dev/oaf/internal/gen/oaf/node/v1/nodev1connect"
)

// DefaultPort is the node agent's mTLS port.
const DefaultPort = 7701

// maxMessage allows a full project (MaxProjectBytes) plus protobuf overhead.
const maxMessage = MaxProjectBytes + 1<<20

// Handler mounts the NodeService on an http.Handler.
func Handler(s *Server) http.Handler {
	srv := connect.NewServer()
	nodev1connect.RegisterNodeServiceHandler(srv, s)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv, connecthttp.WithReadMaxBytes(maxMessage))
	return mux
}

// NewHTTPServer returns the agent's HTTPS server. tlsConfig must come from
// pki.ServerTLS, which requires the oafd client certificate.
func NewHTTPServer(addr string, s *Server, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           Handler(s),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		// Applying a project waits for image pulls, so no write timeout.
	}
}

// NewClient returns a NodeService client for https://addr. tlsConfig must
// come from pki.ClientTLS with the node's name as server name.
func NewClient(addr string, tlsConfig *tls.Config) nodev1connect.NodeServiceClient {
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}}
	return nodev1connect.NewNodeServiceClient(connect.NewClient(
		connecthttp.NewTransport(hc, "https://"+addr, connecthttp.WithReadMaxBytes(maxMessage)),
	))
}
