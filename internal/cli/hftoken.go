package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"openagentfabric.dev/oaf/internal/render"
	"openagentfabric.dev/oaf/internal/secrets"
)

// SecretHFToken is the secret holding a Hugging Face access token for gated
// models: `printf %s "$HF_TOKEN" | sudo oaf secrets set hf-token`.
const SecretHFToken = "hf-token"

// hfToken is the Hugging Face access token for gated weights: HF_TOKEN in
// the environment, else the hf-token secret; "" when there is neither.
func hfToken(secretsDir string) string {
	if t := strings.TrimSpace(os.Getenv("HF_TOKEN")); t != "" {
		return t
	}
	v, ok, err := secrets.Store{Dir: secretsDir}.Lookup(SecretHFToken)
	if err != nil || !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// withHFToken gives gated weights the token so the node agent can download
// them. Only gated repos get it: the token never travels for a download
// that does not need it.
func withHFToken(w *render.Weights, token string) {
	if w != nil && w.Gated {
		w.Token = token
	}
}

// attachHFToken sets the token on every gated project's weights and warns,
// once per model, when there is none.
func attachHFToken(projects []render.Project, token string, out io.Writer) {
	warned := map[string]bool{}
	for _, p := range projects {
		withHFToken(p.Weights, token)
		if p.Weights != nil && p.Weights.Gated && token == "" && !warned[p.Weights.ModelID] {
			warned[p.Weights.ModelID] = true
			_, _ = fmt.Fprintf(out, "warning: %s (%s) is gated on Hugging Face and there is no access token: its download fails until HF_TOKEN is set or `oaf secrets set %s` stores one (from an account that accepted its licence)\n",
				p.Weights.ModelID, p.Weights.Repo, SecretHFToken)
		}
	}
}
