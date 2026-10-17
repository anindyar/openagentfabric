// Package config loads, schema-validates and semantically validates
// cluster.yaml and catalog/models.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"openagentfabric.dev/oaf/schema"
)

var (
	compileOnce sync.Once
	clusterSch  *jsonschema.Schema
	catalogSch  *jsonschema.Schema
	compileErr  error
)

func schemas() (cluster, catalog *jsonschema.Schema, err error) {
	compileOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		for file, id := range map[string]string{
			schema.ClusterFile: schema.ClusterID,
			schema.CatalogFile: schema.CatalogID,
		} {
			raw, err := schema.FS.ReadFile(file)
			if err != nil {
				compileErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				compileErr = fmt.Errorf("%s: %w", file, err)
				return
			}
			if err := c.AddResource(id, doc); err != nil {
				compileErr = err
				return
			}
		}
		if clusterSch, compileErr = c.Compile(schema.ClusterID); compileErr != nil {
			return
		}
		catalogSch, compileErr = c.Compile(schema.CatalogID)
	})
	return clusterSch, catalogSch, compileErr
}

// SchemaError lists every schema violation in a document.
type SchemaError struct {
	Doc    string
	Issues []Issue
}

func (e *SchemaError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s does not match its schema:", e.Doc)
	for _, i := range e.Issues {
		fmt.Fprintf(&b, "\n  %s", i)
	}
	return b.String()
}

// ParseCluster schema-validates and decodes cluster.yaml content.
func ParseCluster(data []byte) (*Cluster, error) {
	cs, _, err := schemas()
	if err != nil {
		return nil, err
	}
	var c Cluster
	if err := parse(data, "cluster.yaml", cs, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ParseCatalog schema-validates and decodes catalog/models.yaml content.
func ParseCatalog(data []byte) (*Catalog, error) {
	_, ks, err := schemas()
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := parse(data, "catalog", ks, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadCluster reads and parses a cluster.yaml file.
func LoadCluster(path string) (*Cluster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCluster(data)
}

// LoadCatalog reads and parses a catalog file.
func LoadCatalog(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCatalog(data)
}

func parse(data []byte, doc string, sch *jsonschema.Schema, out any) error {
	inst, err := yamlToJSONValue(data)
	if err != nil {
		return fmt.Errorf("%s: %w", doc, err)
	}
	if err := sch.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return fmt.Errorf("%s: %w", doc, err)
		}
		return &SchemaError{Doc: doc, Issues: schemaIssues(ve)}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", doc, err)
	}
	return nil
}

// schemaIssues flattens a validation error tree to its leaves, which carry
// the actionable messages, sorted by location.
func schemaIssues(ve *jsonschema.ValidationError) []Issue {
	printer := message.NewPrinter(language.English)
	var issues []Issue
	seen := map[string]bool{}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		path := locationToPath(e.InstanceLocation)
		msg := e.ErrorKind.LocalizedString(printer)
		if key := path + "\x00" + msg; !seen[key] {
			seen[key] = true
			issues = append(issues, Issue{Path: path, Message: msg})
		}
	}
	walk(ve)
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Path < issues[j].Path })
	return issues
}

// locationToPath turns ["spec","models","0","name"] into "spec.models[0].name".
func locationToPath(loc []string) string {
	var b strings.Builder
	for _, tok := range loc {
		if isIndex(tok) {
			fmt.Fprintf(&b, "[%s]", tok)
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(tok)
	}
	return b.String()
}

func isIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
