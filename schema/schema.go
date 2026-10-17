// Package schema embeds the JSON Schemas for cluster.yaml and catalog/models.yaml.
package schema

import "embed"

// FS holds the schema files.
//
//go:embed *.schema.json
var FS embed.FS

// Schema file names and their canonical $id URLs.
const (
	ClusterFile = "cluster.schema.json"
	CatalogFile = "catalog.schema.json"
	ClusterID   = "https://openagentfabric.dev/schema/v1alpha1/cluster.schema.json"
	CatalogID   = "https://openagentfabric.dev/schema/v1alpha1/catalog.schema.json"
)
