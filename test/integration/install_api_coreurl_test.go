// ADR 0113 amendment 1: coreUrl joins the shared identity settings core supplies to every
// annotated module install. No published module chart maps it yet (each adds the annotation in
// its own PR, per the amendment's own list) -- this test is a local, never-published fixture
// chart that does, so core's actual install path (lifecycle.Manager.Install, the same call
// internal/api's install-API handler makes) is proven end to end against a real (test)
// Kubernetes API server, rather than only against the merge function in isolation
// (internal/lifecycle/identity_test.go already covers that).
package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/projectbooth/booth-core/internal/lifecycle"
)

// writeKubeconfig materializes cfg as a kubeconfig file and points the KUBECONFIG environment
// variable at it for the duration of the test. lifecycle.NewManager resolves its cluster
// connection the same way the real `helm` CLI does (helm.sh/helm/v3/pkg/cli.New(), which in turn
// defers to client-go's own default loading rules) -- there is no constructor parameter to hand
// it a *rest.Config directly, so this is how a test points it at envtest instead of whatever
// ambient kubeconfig (or none) the process would otherwise find.
func writeKubeconfig(t *testing.T, cfg *rest.Config) {
	t.Helper()

	kubeCfg := clientcmdapi.NewConfig()
	cluster := clientcmdapi.NewCluster()
	cluster.Server = cfg.Host
	cluster.CertificateAuthorityData = cfg.CAData
	kubeCfg.Clusters["envtest"] = cluster

	authInfo := clientcmdapi.NewAuthInfo()
	authInfo.ClientCertificateData = cfg.CertData
	authInfo.ClientKeyData = cfg.KeyData
	authInfo.Token = cfg.BearerToken
	kubeCfg.AuthInfos["envtest"] = authInfo

	kubeCtx := clientcmdapi.NewContext()
	kubeCtx.Cluster = "envtest"
	kubeCtx.AuthInfo = "envtest"
	kubeCfg.Contexts["envtest"] = kubeCtx
	kubeCfg.CurrentContext = "envtest"

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kubeCfg, path); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
}

// writeCoreURLFixtureChart writes a minimal chart, local to this test only, that annotates
// coreUrl -- contracts/module-manifest.md's "Chart annotation: identity keys" shape, naming only
// the one setting this test cares about.
func writeCoreURLFixtureChart(t *testing.T, dir string) {
	t.Helper()

	chartYAML := "apiVersion: v2\n" +
		"name: coreurl-fixture\n" +
		"version: 0.1.0\n" +
		"annotations:\n" +
		"  booth.projectbooth.io/identity-keys: |\n" +
		"    {\"coreUrl\":\"app.coreUrl\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		t.Fatalf("writing Chart.yaml: %v", err)
	}

	valuesYAML := "app:\n  coreUrl: \"\"\n"
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(valuesYAML), 0o644); err != nil {
		t.Fatalf("writing values.yaml: %v", err)
	}

	templatesDir := filepath.Join(dir, "templates")
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		t.Fatalf("creating templates dir: %v", err)
	}
	cm := "apiVersion: v1\n" +
		"kind: ConfigMap\n" +
		"metadata:\n" +
		"  name: coreurl-fixture\n" +
		"data:\n" +
		"  coreUrl: {{ .Values.app.coreUrl | quote }}\n"
	if err := os.WriteFile(filepath.Join(templatesDir, "configmap.yaml"), []byte(cm), 0o644); err != nil {
		t.Fatalf("writing template: %v", err)
	}
}

// TestManagerInstall_SuppliesCoreURLForAnnotatedChart exercises the exact call
// internal/api's install-API handler makes (lifecycle.Manager.Install, merge included) against a
// real Kubernetes API server, for a chart that annotates coreUrl -- proving the shared setting
// reaches a real install, not only the merge function tested in isolation.
func TestManagerInstall_SuppliesCoreURLForAnnotatedChart(t *testing.T) {
	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })
	writeKubeconfig(t, cfg)

	dir := t.TempDir()
	writeCoreURLFixtureChart(t, dir)

	const namespace = "coreurl-check"
	mgr, err := lifecycle.NewManager(namespace)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	identity := lifecycle.IdentitySettings{CoreURL: "http://booth-core.booth-system.svc.cluster.local:8080"}
	merged, err := mgr.Install("coreurl-fixture", lifecycle.ChartRef{Path: dir}, map[string]any{}, identity)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(merged) != 1 || merged[0] != "coreUrl" {
		t.Fatalf("merged = %v, want exactly [coreUrl]", merged)
	}

	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "coreurl-fixture"}, &out); err != nil {
		t.Fatalf("getting the installed ConfigMap: %v", err)
	}
	if out.Data["coreUrl"] != identity.CoreURL {
		t.Errorf("installed ConfigMap coreUrl = %q, want %q (core's own in-cluster address)", out.Data["coreUrl"], identity.CoreURL)
	}

	// A caller who already set app.coreUrl keeps it, same as every other shared setting
	// (per-leaf, not per-block) -- re-installing (an upgrade, ADR 0113 point 10) with an
	// explicit value must not clobber it.
	explicit := map[string]any{"app": map[string]any{"coreUrl": "http://operator-chosen-address:8080"}}
	merged, err = mgr.Install("coreurl-fixture", lifecycle.ChartRef{Path: dir}, explicit, identity)
	if err != nil {
		t.Fatalf("Install (upgrade with explicit value): %v", err)
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want nothing merged since app.coreUrl was already explicit", merged)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "coreurl-fixture"}, &out); err != nil {
		t.Fatalf("getting the upgraded ConfigMap: %v", err)
	}
	if out.Data["coreUrl"] != "http://operator-chosen-address:8080" {
		t.Errorf("upgraded ConfigMap coreUrl = %q, want the operator's explicit value preserved", out.Data["coreUrl"])
	}
}
