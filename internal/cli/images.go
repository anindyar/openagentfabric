package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/bundle"
	"openagentfabric.dev/oaf/internal/config"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/node"
	"openagentfabric.dev/oaf/internal/pki"
	"openagentfabric.dev/oaf/internal/render"
)

// registryTLS returns the image registry's certificate, issuing it from
// OAF's CA the first time. Only the control node holds the CA key; elsewhere
// it returns nil (no registry is rendered there).
func registryTLS(pkiDir string, c *config.Cluster) (*render.RegistryTLS, []byte, error) {
	host := c.RegistryHost()
	if host == "" {
		return nil, nil, nil
	}
	if _, err := os.Stat(filepath.Join(pkiDir, pki.CAKeyFile)); err != nil {
		return nil, nil, nil
	}
	ca, err := pki.Load(pkiDir, "ca")
	if err != nil {
		return nil, nil, err
	}
	addr, _, _ := net.SplitHostPort(host)
	if pair, err := pki.Load(pkiDir, "registry"); err == nil && certCovers(pair.CertPEM, addr) {
		return &render.RegistryTLS{Cert: pair.CertPEM, Key: pair.KeyPEM}, ca.CertPEM, nil
	}
	opts := pki.LeafOptions{CommonName: "oaf-registry", Server: true}
	if ip := net.ParseIP(addr); ip != nil {
		opts.IPs = []net.IP{ip}
	} else {
		opts.DNSNames = []string{addr}
	}
	pair, err := pki.Issue(ca, opts)
	if err != nil {
		return nil, nil, err
	}
	if err := pki.Save(pkiDir, "registry", pair); err != nil {
		return nil, nil, err
	}
	return &render.RegistryTLS{Cert: pair.CertPEM, Key: pair.KeyPEM}, ca.CertPEM, nil
}

// certCovers reports whether the certificate is valid for addr for at least
// another month.
func certCovers(certPEM []byte, addr string) bool {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return false
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil || time.Until(cert.NotAfter) < 30*24*time.Hour {
		return false
	}
	return cert.VerifyHostname(addr) == nil
}

// trustRegistry makes every node's docker trust OAF's CA for the registry,
// so they can pull the images pinned there.
func (s *stack) trustRegistry(ctx context.Context, out io.Writer) error {
	if s.registry == nil {
		return nil
	}
	host := s.cluster.RegistryHost()
	var errs []error
	for name, cl := range s.nodes {
		resp, err := cl.TrustRegistry(ctx, &nodev1.TrustRegistryRequest{Host: host, CaPem: s.caPEM})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: trusting the image registry: %w", name, err))
			continue
		}
		if resp.GetChanged() {
			_, _ = fmt.Fprintf(out, "trusted   %s  image registry %s\n", name, host)
		}
	}
	return errors.Join(errs...)
}

func newImagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "images",
		Short: "Images of bundle components without a published build (the control node's registry)",
	}
	cmd.AddCommand(newImagesPushCmd(), newImagesPinCmd())
	return cmd
}

func newImagesPushCmd() *cobra.Command {
	var component, cluster, pkiDir, registry string
	cmd := &cobra.Command{
		Use:   "push IMAGE --component NAME",
		Short: "Push a local image to the control node's registry and pin it for a bundle component",
		Long: "Tags IMAGE (one docker on this box has) into the control node's registry as\n" +
			"<registry>/oaf/<component>, pushes it and prints the pinned reference. On the control node\n" +
			"it also pins it in cluster.yaml (spec.images); elsewhere run `oaf images pin` there.\n" +
			"Run it on a box that already has the image: a box that pushed it never pulls it again.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if err := knownComponent(component); err != nil {
				return err
			}
			c, err := config.LoadCluster(cluster)
			if registry == "" {
				if err != nil {
					return fmt.Errorf("--registry host:port is needed off the control node (%w)", err)
				}
				registry = c.RegistryHost()
			}
			caPEM, err := os.ReadFile(filepath.Join(pkiDir, pki.CACertFile))
			if err != nil {
				return fmt.Errorf("OAF's CA (%w)", err)
			}
			srv := &node.Server{}
			if _, err := srv.TrustRegistry(cmd.Context(), &nodev1.TrustRegistryRequest{Host: registry, CaPem: caPEM}); err != nil {
				return fmt.Errorf("trusting %s: %w", registry, err)
			}
			ref, err := pushImage(cmd.Context(), out, args[0], registry, component)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "\npushed %s\n", ref)
			if c != nil && c.RegistryHost() == registry {
				if err := pinImage(cluster, component, ref); err == nil {
					_, _ = fmt.Fprintf(out, "pinned %s in %s; run `oaf apply` (or approve a change in the console) to use it\n", component, cluster)
					return nil
				}
			}
			_, _ = fmt.Fprintf(out, "on the control node run:  sudo oaf images pin %s %s\n", component, ref)
			return nil
		},
	}
	cmd.Flags().StringVar(&component, "component", "", "bundle component the image is for (required)")
	cmd.Flags().StringVar(&cluster, "file", "/etc/oaf/cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&pkiDir, "pki-dir", DefaultPKIDir, "directory with OAF's ca.crt")
	cmd.Flags().StringVar(&registry, "registry", "", "registry host:port (default: the control node's, from cluster.yaml)")
	_ = cmd.MarkFlagRequired("component")
	return cmd
}

func newImagesPinCmd() *cobra.Command {
	var cluster string
	cmd := &cobra.Command{
		Use:   "pin COMPONENT REFERENCE",
		Short: "Pin a pushed image (registry/repo@sha256:...) for a bundle component in cluster.yaml",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := knownComponent(args[0]); err != nil {
				return err
			}
			if err := pinImage(cluster, args[0], args[1]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "pinned %s; run `oaf apply` (or approve a change in the console) to use it\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&cluster, "file", "/etc/oaf/cluster.yaml", "path to cluster.yaml")
	return cmd
}

func knownComponent(name string) error {
	v, err := config.ParseVersions(bundle.VersionsYAML)
	if err != nil {
		return err
	}
	for _, c := range v.Components {
		if c.Name == name {
			return nil
		}
	}
	return fmt.Errorf("bundle %s has no component %q", v.Version, name)
}

// pinImage sets spec.images.<component> in cluster.yaml after checking the
// result is valid.
func pinImage(path, component, ref string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, _, err := config.SetSpecString(data, ref, "images", component)
	if err != nil {
		return err
	}
	c, err := config.ParseCluster(out)
	if err != nil {
		return err
	}
	cat, err := config.LoadCatalog(DefaultCatalogPath)
	if err == nil {
		if rep := config.Validate(c, cat, config.Options{}); !rep.OK() {
			return fmt.Errorf("cluster.yaml would not be valid: %v", rep.Errors())
		}
	}
	return config.WriteFileAtomic(path, out)
}

var digestLine = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})`)

// pushImage tags image as <registry>/oaf/<component>:<tag>, pushes it and
// returns <registry>/oaf/<component>@sha256:<digest>.
func pushImage(ctx context.Context, out io.Writer, image, registry, component string) (string, error) {
	tag := "local"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		tag = image[i+1:]
	}
	repo := registry + "/oaf/" + component
	if b, err := exec.CommandContext(ctx, "docker", "tag", image, repo+":"+tag).CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker tag: %v: %s", err, bytes.TrimSpace(b))
	}
	var buf bytes.Buffer
	push := exec.CommandContext(ctx, "docker", "push", repo+":"+tag)
	push.Stdout, push.Stderr = io.MultiWriter(out, &buf), io.MultiWriter(out, &buf)
	if err := push.Run(); err != nil {
		return "", fmt.Errorf("docker push: %w", err)
	}
	if m := digestLine.FindStringSubmatch(buf.String()); m != nil {
		return repo + "@" + m[1], nil
	}
	b, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", repo+":"+tag).Output()
	if err != nil {
		return "", fmt.Errorf("docker image inspect: %w", err)
	}
	var digests []string
	_ = json.Unmarshal(b, &digests)
	for _, d := range digests {
		if strings.HasPrefix(d, repo+"@") {
			return d, nil
		}
	}
	return "", errors.New("the push reported no digest")
}
