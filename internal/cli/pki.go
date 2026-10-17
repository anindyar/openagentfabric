package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/pki"
)

func newPKICmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pki", Short: "Manage the cluster's mTLS certificates"}
	cmd.AddCommand(newPKIInitCmd())
	return cmd
}

func newPKIInitCmd() *cobra.Command {
	var dir, cluster string
	var nodes []string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a cluster CA, the oafd client certificate and a certificate per node",
		Long: "Writes <dir>/ca.{crt,key}, <dir>/oafd.{crt,key} and <dir>/nodes/<name>/{ca.crt,node.crt,node.key}.\n" +
			"Each --node is name=address[,address...]. Refuses to overwrite an existing CA.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := os.Stat(filepath.Join(dir, pki.CACertFile)); err == nil {
				return fmt.Errorf("%s already has a CA; refusing to overwrite it", dir)
			}
			ca, err := pki.NewCA(cluster)
			if err != nil {
				return err
			}
			if err := pki.Save(dir, "ca", ca); err != nil {
				return err
			}
			oafd, err := pki.Issue(ca, pki.LeafOptions{CommonName: pki.ControlPlaneCN, Client: true})
			if err != nil {
				return err
			}
			if err := pki.Save(dir, "oafd", oafd); err != nil {
				return err
			}
			for _, spec := range nodes {
				name, addrs, _ := strings.Cut(spec, "=")
				o := pki.LeafOptions{CommonName: name, DNSNames: []string{name}, Server: true}
				for _, a := range strings.Split(addrs, ",") {
					if ip := net.ParseIP(strings.TrimSpace(a)); ip != nil {
						o.IPs = append(o.IPs, ip)
					} else if a != "" {
						o.DNSNames = append(o.DNSNames, a)
					}
				}
				leaf, err := pki.Issue(ca, o)
				if err != nil {
					return err
				}
				nd := filepath.Join(dir, "nodes", name)
				if err := pki.Save(nd, "node", leaf); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(nd, pki.CACertFile), ca.CertPEM, 0o644); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote CA, oafd and %d node certificate(s) to %s\n", len(nodes), dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "/etc/oaf/pki", "output directory")
	cmd.Flags().StringVar(&cluster, "cluster", "oaf", "cluster name (for the CA subject)")
	cmd.Flags().StringArrayVar(&nodes, "node", nil, "node certificate to issue: name=address[,address...] (repeatable)")
	return cmd
}
