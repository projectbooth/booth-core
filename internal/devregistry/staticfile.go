// Package devregistry is booth-core's local-development fallback for module discovery
// (ADR 0019's "worth building as a convenience, not a blocker" item, and
// agent-briefs/core.md's third open question). Instead of watching BoothModule CRDs
// against a real cluster, it loads the same manifest shape from a static YAML file, so a
// module can be developed and pointed at a running core without kind/k3d.
//
// This is a development convenience only — it never runs against a real deployment, and
// modules loaded this way get no health polling loop, no status subresource, and no
// reconciliation. It exists purely so a module agent (or booth-core itself) can start the
// gateway/API locally with a believable registry.
package devregistry

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/registry"
)

// fileEntry mirrors contracts/module-manifest.md's fields plus the ServiceRef
// booth-core needs to route to it — the same shape a BoothModule.Spec carries, just
// authored by hand in YAML instead of templated by a Helm chart.
type fileEntry struct {
	ID                string                          `yaml:"id"`
	DisplayName       string                          `yaml:"displayName"`
	Icon              string                          `yaml:"icon"`
	Version           string                          `yaml:"version"`
	ContractVersion   string                          `yaml:"contractVersion"`
	HasOwnUI          bool                            `yaml:"hasOwnUi"`
	UIIntegrationMode boothv1alpha1.UIIntegrationMode `yaml:"uiIntegrationMode"`
	HealthCheckPath   string                          `yaml:"healthCheckPath"`
	RequiredScopes    []string                        `yaml:"requiredScopes"`
	NavPath           string                          `yaml:"navPath"`
	NavGroup          boothv1alpha1.NavGroup          `yaml:"navGroup"`
	AdminNavPath      string                          `yaml:"adminNavPath"`
	// Host is a directly-dialable address (e.g. "localhost:9001") for local dev,
	// replacing the in-cluster Service DNS name a real BoothModule's ServiceRef
	// resolves to.
	Host string `yaml:"host"`
}

type fileFormat struct {
	Modules []fileEntry `yaml:"modules"`
}

// Load reads a static registry file and returns the modules it describes, marked
// Healthy unconditionally — dev mode trusts the file rather than polling, since the
// whole point is running without a cluster the health-check DNS assumes.
func Load(path string) ([]registry.Module, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading dev registry file %s: %w", path, err)
	}

	var parsed fileFormat
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parsing dev registry file %s: %w", path, err)
	}

	modules := make([]registry.Module, 0, len(parsed.Modules))
	for _, e := range parsed.Modules {
		if e.ID == "" {
			return nil, fmt.Errorf("dev registry file %s: entry missing required field id", path)
		}
		modules = append(modules, registry.Module{
			Spec: boothv1alpha1.BoothModuleSpec{
				ID:                e.ID,
				DisplayName:       e.DisplayName,
				Icon:              e.Icon,
				Version:           e.Version,
				ContractVersion:   e.ContractVersion,
				HasOwnUI:          e.HasOwnUI,
				UIIntegrationMode: e.UIIntegrationMode,
				HealthCheckPath:   e.HealthCheckPath,
				RequiredScopes:    e.RequiredScopes,
				NavPath:           e.NavPath,
				NavGroup:          e.NavGroup,
				AdminNavPath:      e.AdminNavPath,
				ServiceRef: boothv1alpha1.ServiceReference{
					Name: e.Host,
				},
			},
			Status: boothv1alpha1.BoothModuleStatus{
				Phase: boothv1alpha1.ModulePhaseHealthy,
			},
		})
	}

	return modules, nil
}
