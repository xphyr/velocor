// Package monitoring embeds the shipped alert rules, so the appliance's built-in Prometheus
// runs exactly the file promtool tests (hack/check-alerts.sh) and docs/monitoring.md describes.
package monitoring

import _ "embed"

// Rules is velocor-alerts.rules.yml.
//
//go:embed velocor-alerts.rules.yml
var Rules []byte
