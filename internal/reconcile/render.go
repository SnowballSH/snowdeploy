// Package reconcile turns a manifest into a running service: render the
// Quadlet unit, refuse it if the rendered text breaks the safety rules, write
// it, restart, and probe.
package reconcile

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/SnowballSH/snowdeploy/internal/manifest"
)

// UnitName is the Quadlet file a service is rendered into.
func UnitName(service string) string { return service + ".container" }

// Render fills a Quadlet template from a manifest. A reference to a field the
// manifest does not have is an error, never an empty string.
func Render(tmpl string, m *manifest.Manifest) (string, error) {
	t, err := template.New("unit").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, m); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	return buf.String(), nil
}
