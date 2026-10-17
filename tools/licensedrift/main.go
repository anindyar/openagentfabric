// Command licensedrift checks that each pinned upstream's licence file still
// hashes to the value recorded in bundle/versions.yaml (docs/licence-audit.md §4.3).
// A mismatch means the pin and the audit disagree: re-audit before releasing.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"openagentfabric.dev/oaf/bundle"
	"openagentfabric.dev/oaf/internal/config"
)

func main() {
	v, err := config.ParseVersions(bundle.VersionsYAML)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	failed := 0
	for _, c := range v.Components {
		got, err := fetchSHA256(client, c.LicenseFileURL())
		switch {
		case err != nil:
			fmt.Printf("ERROR  %-16s %v\n", c.Name, err)
			failed++
		case got != c.License.SHA256:
			fmt.Printf("DRIFT  %-16s %s at %s: sha256 %s, pinned %s\n", c.Name, c.License.File, c.Upstream.Ref, got, c.License.SHA256)
			failed++
		default:
			fmt.Printf("ok     %-16s %s %s@%s\n", c.Name, c.License.SPDX, c.Upstream.Repo, c.Upstream.Ref)
		}
	}
	if failed > 0 {
		fmt.Printf("%d component(s) need a licence re-audit\n", failed)
		os.Exit(1)
	}
}

func fetchSHA256(client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
