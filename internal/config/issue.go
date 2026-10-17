package config

import "fmt"

// Severity of a validation issue.
type Severity int

// Severities.
const (
	SeverityError Severity = iota
	SeverityWarning
)

// Issue is one problem found in a document. Path uses dotted notation,
// e.g. spec.models[1].placement.group.
type Issue struct {
	Path     string
	Message  string
	Severity Severity
}

func (i Issue) String() string {
	if i.Path == "" {
		return i.Message
	}
	return fmt.Sprintf("%s: %s", i.Path, i.Message)
}
