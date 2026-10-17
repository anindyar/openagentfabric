// Package bundle embeds the pinned component set (versions.yaml) into the oaf binary.
package bundle

import _ "embed"

// VersionsYAML is bundle/versions.yaml.
//
//go:embed versions.yaml
var VersionsYAML []byte
