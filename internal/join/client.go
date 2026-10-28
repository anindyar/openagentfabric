package join

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// Client is the joining box's side: it talks to oafd with the join token,
// trusting only oafd's pinned TLS key.
type Client struct {
	Control string // https://host:8443
	Token   string
	HTTP    *http.Client
}

// PinnedHTTP is an HTTP client that accepts only a server whose public key
// matches pin (sha256//<base64>), whatever its certificate chain.
func PinnedHTTP(pin string, timeout time.Duration) (*http.Client, error) {
	want, ok := strings.CutPrefix(pin, "sha256//")
	if !ok {
		return nil, errors.New("pin must look like sha256//<base64>")
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // replaced by the key pin below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no server certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if base64.StdEncoding.EncodeToString(sum[:]) != want {
				return errors.New("the control node's TLS key does not match the pin in the join command")
			}
			return nil
		},
	}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: cfg, Proxy: http.ProxyFromEnvironment}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Control, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s %s: %s", method, path, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Images lists the bundle images the control node offers.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var out struct {
		Images []Image `json:"images"`
	}
	return out.Images, c.do(ctx, http.MethodGet, "/api/v1/join/images", nil, &out)
}

// LoadImage streams one image from the control node into `docker load`.
func (c *Client) LoadImage(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.Control, "/")+"/api/v1/join/images/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("image %s: %s", name, resp.Status)
	}
	cmd := exec.CommandContext(ctx, "docker", "load")
	cmd.Stdin = resp.Body
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker load %s: %w: %s", name, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Enrol uses the token up and returns the node's certificate.
func (c *Client) Enrol(ctx context.Context, req EnrolRequest) (EnrolResponse, error) {
	var out EnrolResponse
	return out, c.do(ctx, http.MethodPost, "/api/v1/join/enrol", req, &out)
}

// NodeUnit is deploy/systemd/oaf-node.service (kept identical by a test), so
// a joined box runs the agent exactly as an installed one does.
const NodeUnit = `# Installed by ` + "`oaf install`" + ` on every node.
[Unit]
Description=openagentfabric node agent
Documentation=https://github.com/anindyar/openagentfabric
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
ExecStart=/usr/local/bin/oaf node serve --pki-dir /etc/oaf/pki --state-dir /var/lib/oaf
Restart=on-failure
RestartSec=5
# Runs as root: it drives docker and configures the QSFP fabric.
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=read-only
PrivateTmp=yes
# systemd creates /var/lib/oaf; ReadWritePaths needs existing paths ("-" skips a missing one).
StateDirectory=oaf
StateDirectoryMode=0755
ReadWritePaths=-/etc/oaf
# The fabric: oaf-node writes only /etc/netplan/40-oaf-fabric.yaml there.
ReadWritePaths=-/etc/netplan
# Docker's trust for OAF's image registry: only /etc/docker/certs.d/<registry>/ca.crt.
ReadWritePaths=-/etc/docker

[Install]
WantedBy=multi-user.target
`
