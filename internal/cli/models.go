package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/config"
)

func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "models", Short: "Edit the model deployments in cluster.yaml"}
	cmd.AddCommand(newModelsAddCmd())
	return cmd
}

func newModelsAddCmd() *cobra.Command {
	var clusterPath, catalogPath, ref, node string
	var util float64
	var maxLen int
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a model deployment to cluster.yaml (idempotent)",
		Long: "Adds spec.models entry <name> serving catalog model --catalog-ref, unless a deployment\n" +
			"of that name exists. Comments in cluster.yaml are kept. The result is validated against\n" +
			"the catalog, memory budget included, before it is written; run `oaf apply` to deploy it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := config.LoadCatalog(catalogPath)
			if err != nil {
				return err
			}
			m, ok := cat.Lookup(ref)
			if !ok {
				return fmt.Errorf("the catalog has no model %q", ref)
			}
			data, err := os.ReadFile(clusterPath)
			if err != nil {
				return err
			}
			c, err := config.ParseCluster(data)
			if err != nil {
				return err
			}
			if node == "" {
				node = firstInferenceNode(c)
			}
			d := config.ModelDeployment{Name: args[0], CatalogRef: ref, Placement: config.Placement{Node: node}, GPUMemoryUtilization: util, MaxModelLen: maxLen}
			if p, err := config.Profile(d, m, 1); err == nil {
				if d.GPUMemoryUtilization == 0 {
					d.GPUMemoryUtilization = p.GPUMemoryUtilization
				}
				if d.MaxModelLen == 0 {
					d.MaxModelLen = p.MaxModelLen
				}
			}
			out, changed, err := config.AddModel(data, d)
			if err != nil {
				return err
			}
			if !changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s already deploys %q\n", clusterPath, args[0])
				return err
			}
			next, err := config.ParseCluster(out)
			if err != nil {
				return err
			}
			r := config.Validate(next, cat, config.Options{})
			for _, w := range r.Warnings() {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
			if !r.OK() {
				for _, e := range r.Errors() {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "error:   %s\n", e)
				}
				return errors.New("not added: the result would not validate")
			}
			if err := config.WriteFileAtomic(clusterPath, out); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "added %q (%s on %s, gpuMemoryUtilization %g); run `oaf apply` to deploy it\n",
				args[0], ref, node, d.GPUMemoryUtilization)
			return err
		},
	}
	cmd.Flags().StringVarP(&clusterPath, "file", "f", DefaultClusterPath, "path to cluster.yaml")
	cmd.Flags().StringVar(&catalogPath, "catalog", DefaultCatalogPath, "path to the model catalog")
	cmd.Flags().StringVar(&ref, "catalog-ref", "", "catalog model id (required)")
	cmd.Flags().StringVar(&node, "node", "", "node to place it on (default: the first inference node)")
	cmd.Flags().Float64Var(&util, "util", 0, "gpuMemoryUtilization (default: the catalog profile's)")
	cmd.Flags().IntVar(&maxLen, "max-model-len", 0, "context length (default: the catalog profile's)")
	_ = cmd.MarkFlagRequired("catalog-ref")
	return cmd
}

func firstInferenceNode(c *config.Cluster) string {
	for _, n := range c.Spec.Nodes {
		if n.HasRole(config.RoleInference) {
			return n.Name
		}
	}
	if len(c.Spec.Nodes) > 0 {
		return c.Spec.Nodes[0].Name
	}
	return ""
}
