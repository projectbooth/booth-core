// Package lifecycle implements booth-core's module install/uninstall lifecycle
// (ADR 0003, agent-briefs/core.md's v0 definition of done): installing or removing a
// module's Helm chart. A successful install's chart is expected to template a
// BoothModule resource per ADR 0019, which internal/registry's controller then picks up
// on its own — this package's job ends at "the chart is installed/removed," not at
// updating the registry directly.
//
// Scope note: where the *desired* install state comes from (a Module Store UI, a CLI, a
// GitOps file) is intentionally left to the caller of Manager rather than decided here —
// see docs/decisions/0005-module-lifecycle-desired-state.md for why that's flagged back
// rather than settled unilaterally in this package.
package lifecycle

import (
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
)

// ChartRef locates a module's Helm chart: either a local path (a pre-fetched tarball or
// directory, useful for tests and air-gapped installs) or a repo-hosted chart resolved
// via RepoURL+Version the same way `helm install --repo ... chart --version ...` would.
type ChartRef struct {
	// Path, if set, is used as-is via helm's chart loader — takes precedence over
	// RepoURL/Version.
	Path string

	// RepoURL + ChartName + Version locate a chart the same way the helm CLI's
	// --repo flag does, when Path is empty.
	RepoURL   string
	ChartName string
	Version   string
}

// Manager wraps Helm's Go SDK to install/uninstall one module's chart into its own
// namespace. One Manager instance is namespace-scoped; booth-core constructs one per
// install/uninstall call for whichever namespace that module targets, since Helm's own
// action.Configuration is namespace-scoped.
type Manager struct {
	cfg       *action.Configuration
	namespace string
}

// NewManager builds a Manager targeting namespace, using the ambient kubeconfig/in-cluster
// config (the same resolution `helm` itself uses) and Helm's default Secret-backed
// release storage in that namespace.
func NewManager(namespace string) (*Manager, error) {
	settings := cli.New()
	settings.SetNamespace(namespace)

	cfg := new(action.Configuration)
	if err := cfg.Init(settings.RESTClientGetter(), namespace, "secrets", logf); err != nil {
		return nil, fmt.Errorf("initializing helm action configuration for namespace %s: %w", namespace, err)
	}

	return &Manager{cfg: cfg, namespace: namespace}, nil
}

// Install installs ref as releaseName, creating it if absent or upgrading it in place if
// already installed — the same "install or upgrade" semantics the Module Store's
// "install"/"update version" actions both need, since a module version bump is just a
// re-install of a newer chart version.
func (m *Manager) Install(releaseName string, ref ChartRef, values map[string]any) error {
	chart, err := loadChart(m, ref)
	if err != nil {
		return err
	}

	if m.releaseExists(releaseName) {
		upgrade := action.NewUpgrade(m.cfg)
		upgrade.Namespace = m.namespace
		if _, err := upgrade.Run(releaseName, chart, values); err != nil {
			return fmt.Errorf("upgrading release %s: %w", releaseName, err)
		}
		return nil
	}

	install := action.NewInstall(m.cfg)
	install.ReleaseName = releaseName
	install.Namespace = m.namespace
	install.CreateNamespace = true
	if _, err := install.Run(chart, values); err != nil {
		return fmt.Errorf("installing release %s: %w", releaseName, err)
	}
	return nil
}

// Uninstall removes releaseName. Not finding it is treated as success — uninstalling
// something already gone is the desired end state, not an error.
func (m *Manager) Uninstall(releaseName string) error {
	if !m.releaseExists(releaseName) {
		return nil
	}

	uninstall := action.NewUninstall(m.cfg)
	if _, err := uninstall.Run(releaseName); err != nil {
		return fmt.Errorf("uninstalling release %s: %w", releaseName, err)
	}
	return nil
}

func (m *Manager) releaseExists(releaseName string) bool {
	status := action.NewStatus(m.cfg)
	_, err := status.Run(releaseName)
	return err == nil
}

func loadChart(m *Manager, ref ChartRef) (*chart.Chart, error) {
	if ref.Path != "" {
		c, err := loader.Load(ref.Path)
		if err != nil {
			return nil, fmt.Errorf("loading chart from %s: %w", ref.Path, err)
		}
		return c, nil
	}

	install := action.NewInstall(m.cfg)
	install.RepoURL = ref.RepoURL
	install.Version = ref.Version
	settings := cli.New()
	path, err := install.LocateChart(ref.ChartName, settings)
	if err != nil {
		return nil, fmt.Errorf("locating chart %s from %s: %w", ref.ChartName, ref.RepoURL, err)
	}
	c, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading chart from %s: %w", path, err)
	}
	return c, nil
}

func logf(format string, v ...any) {
	// Structured-enough for stdout logging per ADR 0022; Helm's own action logs are
	// low-volume (one line per lifecycle step), so a plain Printf is proportionate.
	fmt.Printf("[helm] "+format+"\n", v...)
}
