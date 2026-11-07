package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// peftConfig is the part of a PEFT adapter_config.json OAF checks.
type peftConfig struct {
	PEFTType string `json:"peft_type"`
	R        int    `json:"r"`
	Base     string `json:"base_model_name_or_path"`
}

// CheckAdapterFiles compares each adapter in cluster.yaml with its files, as
// far as they can be seen from where this runs: root is the host's
// filesystem (os.DirFS("/")). Adapters live on the inference nodes, so a
// directory missing here is only a warning (it may be on another box); a
// rank larger than declared is an error, because vLLM refuses an adapter
// whose rank exceeds --max-lora-rank.
func CheckAdapterFiles(c *Cluster, cat *Catalog, root fs.FS) []Issue {
	var out []Issue
	add := func(sev Severity, p, format string, args ...any) {
		out = append(out, Issue{Path: p, Message: fmt.Sprintf(format, args...), Severity: sev})
	}
	for i, d := range c.Spec.Models {
		for j, a := range d.Adapters {
			p := fmt.Sprintf("spec.models[%d].adapters[%d]", i, j)
			if !path.IsAbs(a.Path) {
				continue // semantic validation reports it
			}
			dir := strings.TrimPrefix(path.Clean(a.Path), "/")
			raw, err := fs.ReadFile(root, path.Join(dir, "adapter_config.json"))
			if errors.Is(err, fs.ErrNotExist) {
				if _, derr := fs.Stat(root, dir); derr != nil {
					add(SeverityWarning, p+".path", "%s is not on this host; it must exist on every node of %s's placement before apply", a.Path, d.Name)
				} else {
					add(SeverityError, p+".path", "%s has no adapter_config.json; it is not a PEFT adapter directory", a.Path)
				}
				continue
			}
			if err != nil {
				add(SeverityWarning, p+".path", "cannot read %s/adapter_config.json: %v", a.Path, err)
				continue
			}
			var pc peftConfig
			if err := json.Unmarshal(raw, &pc); err != nil {
				add(SeverityError, p+".path", "%s/adapter_config.json is not valid JSON: %v", a.Path, err)
				continue
			}
			if pc.PEFTType != "" && pc.PEFTType != "LORA" {
				add(SeverityError, p+".path", "%s is a %s adapter; vLLM serves LoRA adapters only", a.Path, pc.PEFTType)
			}
			switch {
			case pc.R > a.Rank:
				add(SeverityError, p+".rank", "adapter_config.json has r=%d, but rank is %d: vLLM would refuse the adapter; set rank: %d", pc.R, a.Rank, pc.R)
			case pc.R > 0 && pc.R < a.Rank:
				add(SeverityWarning, p+".rank", "adapter_config.json has r=%d, but rank is %d: vLLM reserves LoRA memory for the larger rank", pc.R, a.Rank)
			}
			if !anyExists(root, path.Join(dir, "adapter_model.safetensors"), path.Join(dir, "adapter_model.bin")) {
				add(SeverityError, p+".path", "%s has no adapter_model.safetensors (or .bin)", a.Path)
			}
			// A hub repo id as the base must be the deployment's model.
			if m, ok := cat.Lookup(d.CatalogRef); ok && m.Source.HuggingFace != nil &&
				pc.Base != "" && !strings.HasPrefix(pc.Base, "/") && strings.Count(pc.Base, "/") == 1 &&
				!strings.EqualFold(pc.Base, m.Source.HuggingFace.Repo) {
				add(SeverityWarning, p, "adapter %q was trained on %s, but %s serves %s", a.Name, pc.Base, d.Name, m.Source.HuggingFace.Repo)
			}
		}
	}
	return out
}

func anyExists(root fs.FS, names ...string) bool {
	for _, n := range names {
		if _, err := fs.Stat(root, n); err == nil {
			return true
		}
	}
	return false
}
