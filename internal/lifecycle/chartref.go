package lifecycle

import (
	"fmt"
	"strings"
)

// IsZero reports whether no chart location has been filled in at all.
func (c ChartRef) IsZero() bool {
	return c.Path == "" && c.RepoURL == "" && c.ChartName == ""
}

// ParseChartRef translates a single chartRef string (contracts/module-registry-protocol.md's
// registry-entry shape) plus its chartVersion into the structured ChartRef Install/Uninstall
// use internally (ADR 0028). Lifted from booth-module-store's own interim
// internal/catalog/chartref.go (ParseRegistryChartRef) per that ADR's instruction, now
// that booth-core is the one place this parsing lives — booth-module-store retires its
// copy once it switches to passing chartRef straight through.
//
// v0 scope, unchanged from the source this was lifted from: only the
// "oci://host/path/chart-name" form is understood (splitting the last path segment off
// as the chart name, the rest as the repo URL). Any other shape is rejected with an
// error rather than guessed at, so a bad assumption fails loudly at install time instead
// of silently installing the wrong chart. Additional schemes can be added here later
// without any caller-side change, since parsing is centralized in booth-core now.
func ParseChartRef(chartRef, chartVersion string) (ChartRef, error) {
	if chartRef == "" {
		return ChartRef{}, fmt.Errorf("chartRef is empty")
	}
	if !strings.HasPrefix(chartRef, "oci://") {
		return ChartRef{}, fmt.Errorf("chartRef %q: only oci:// references are understood by this v0 parser (ADR 0028)", chartRef)
	}

	trimmed := strings.TrimSuffix(chartRef, "/")
	lastSlash := strings.LastIndex(trimmed, "/")
	if lastSlash < len("oci://") {
		return ChartRef{}, fmt.Errorf("chartRef %q: expected oci://host/path/chart-name", chartRef)
	}

	repoURL := trimmed[:lastSlash]
	chartName := trimmed[lastSlash+1:]
	if chartName == "" {
		return ChartRef{}, fmt.Errorf("chartRef %q: empty chart name after last '/'", chartRef)
	}

	return ChartRef{
		RepoURL:   repoURL,
		ChartName: chartName,
		Version:   chartVersion,
	}, nil
}
