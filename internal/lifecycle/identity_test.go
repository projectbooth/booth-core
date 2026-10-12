package lifecycle

import (
	"bytes"
	"log"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
)

func fixtureChart(annotation string) *chart.Chart {
	meta := &chart.Metadata{Name: "fixture"}
	if annotation != "" {
		meta.Annotations = map[string]string{identityKeysAnnotation: annotation}
	}
	return &chart.Chart{Metadata: meta}
}

var fullIdentity = IdentitySettings{
	IssuerURL:         "https://booth.home.arpa/realms/booth",
	ClientID:          "booth-design",
	JWKSURL:           "http://booth-core-keycloak.booth-system.svc.cluster.local:8080/realms/booth/protocol/openid-connect/certs",
	WorkloadIssuerURL: "http://booth-core.booth-system.svc.cluster.local:8080",
	GroupsClaim:       "groups",
	IframeIssuerURL:   "http://booth-core.booth-system.svc.cluster.local:8080/iframe-identity",
	CoreURL:           "http://booth-core.booth-system.svc.cluster.local:8080",
}

const fullMapping = `{"issuerUrl":"oidc.issuerUrl","clientId":"oidc.clientId","jwksUrl":"oidc.jwksUrl","workloadIssuerUrl":"oidc.workloadIssuerUrl","groupsClaim":"oidc.groupsClaim","iframeIssuerUrl":"identity.issuerUrl","coreUrl":"core.url"}`

// captureLog redirects the standard logger for the duration of fn and returns what it wrote,
// so a test can assert the "ignored, logged" cases actually log something, not just skip
// silently.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	fn()
	return buf.String()
}

func TestMergeIdentitySettings_PerLeafNotPerBlock(t *testing.T) {
	c := fixtureChart(fullMapping)
	values := map[string]any{
		"oidc": map[string]any{"clientId": "caller-supplied-client"},
	}

	got, merged := mergeIdentitySettings(c, fullIdentity, values)

	oidc := got["oidc"].(map[string]any)
	if oidc["clientId"] != "caller-supplied-client" {
		t.Errorf("oidc.clientId = %v, want the caller's own value preserved", oidc["clientId"])
	}
	if oidc["issuerUrl"] != fullIdentity.IssuerURL {
		t.Errorf("oidc.issuerUrl = %v, want core's value filled in", oidc["issuerUrl"])
	}
	if oidc["jwksUrl"] != fullIdentity.JWKSURL {
		t.Errorf("oidc.jwksUrl = %v, want core's value filled in", oidc["jwksUrl"])
	}
	if oidc["groupsClaim"] != fullIdentity.GroupsClaim {
		t.Errorf("oidc.groupsClaim = %v, want core's value filled in", oidc["groupsClaim"])
	}
	core := got["core"].(map[string]any)
	if core["url"] != fullIdentity.CoreURL {
		t.Errorf("core.url = %v, want core's own in-cluster address filled in", core["url"])
	}

	for _, name := range merged {
		if name == "clientId" {
			t.Errorf("merged = %v, clientId should not be reported as supplied by core since the caller already set it", merged)
		}
	}
	wantMerged := map[string]bool{"issuerUrl": true, "jwksUrl": true, "workloadIssuerUrl": true, "groupsClaim": true, "iframeIssuerUrl": true, "coreUrl": true}
	if len(merged) != len(wantMerged) {
		t.Errorf("merged = %v, want exactly %v", merged, wantMerged)
	}
	for _, name := range merged {
		if !wantMerged[name] {
			t.Errorf("merged contains unexpected setting %q", name)
		}
	}
}

func TestMergeIdentitySettings_ExplicitValueIsPreservedExactly(t *testing.T) {
	c := fixtureChart(`{"issuerUrl":"oidc.issuerUrl"}`)
	values := map[string]any{
		"oidc": map[string]any{"issuerUrl": "https://external-idp.example.com/realms/booth"},
	}

	got, merged := mergeIdentitySettings(c, fullIdentity, values)

	oidc := got["oidc"].(map[string]any)
	if oidc["issuerUrl"] != "https://external-idp.example.com/realms/booth" {
		t.Errorf("oidc.issuerUrl = %v, want the caller's explicit external-IdP value untouched", oidc["issuerUrl"])
	}
	if len(merged) != 0 {
		t.Errorf("merged = %v, want nothing merged since the one named setting was already explicit", merged)
	}
}

func TestMergeIdentitySettings_UnsetSettingIsSkippedNotWrittenEmpty(t *testing.T) {
	identity := fullIdentity
	identity.WorkloadIssuerURL = "" // workload identity disabled

	c := fixtureChart(`{"issuerUrl":"oidc.issuerUrl","workloadIssuerUrl":"oidc.workloadIssuerUrl"}`)
	got, merged := mergeIdentitySettings(c, identity, map[string]any{})

	oidc := got["oidc"].(map[string]any)
	if _, present := oidc["workloadIssuerUrl"]; present {
		t.Errorf("oidc.workloadIssuerUrl = %v, want the key absent entirely, never an empty string", oidc["workloadIssuerUrl"])
	}
	if oidc["issuerUrl"] != identity.IssuerURL {
		t.Errorf("oidc.issuerUrl = %v, want core's value filled in", oidc["issuerUrl"])
	}
	for _, name := range merged {
		if name == "workloadIssuerUrl" {
			t.Error("merged reports workloadIssuerUrl as supplied, but core has no value for it")
		}
	}
}

// TestMergeIdentitySettings_CoreURLSupplied is ADR 0113 amendment 1's own case: a chart that
// annotates coreUrl gets core's own in-cluster address filled in, reported in merged, exactly
// like every other shared setting.
func TestMergeIdentitySettings_CoreURLSupplied(t *testing.T) {
	c := fixtureChart(`{"coreUrl":"core.url"}`)
	got, merged := mergeIdentitySettings(c, fullIdentity, map[string]any{})

	core := got["core"].(map[string]any)
	if core["url"] != fullIdentity.CoreURL {
		t.Errorf("core.url = %v, want %v", core["url"], fullIdentity.CoreURL)
	}
	if len(merged) != 1 || merged[0] != "coreUrl" {
		t.Errorf("merged = %v, want exactly [coreUrl]", merged)
	}
}

// TestMergeIdentitySettings_CoreURLExplicitValueIsPreservedExactly mirrors
// TestMergeIdentitySettings_ExplicitValueIsPreservedExactly for coreUrl specifically: a caller
// who already points at a different core address (or any other value) keeps it untouched.
func TestMergeIdentitySettings_CoreURLExplicitValueIsPreservedExactly(t *testing.T) {
	c := fixtureChart(`{"coreUrl":"core.url"}`)
	values := map[string]any{
		"core": map[string]any{"url": "http://my-other-core.other-namespace.svc.cluster.local:8080"},
	}

	got, merged := mergeIdentitySettings(c, fullIdentity, values)

	core := got["core"].(map[string]any)
	if core["url"] != "http://my-other-core.other-namespace.svc.cluster.local:8080" {
		t.Errorf("core.url = %v, want the caller's explicit value untouched", core["url"])
	}
	if len(merged) != 0 {
		t.Errorf("merged = %v, want nothing merged since coreUrl was already explicit", merged)
	}
}

// TestMergeIdentitySettings_CoreURLSkippedWhenCoreHasNoValue mirrors
// TestMergeIdentitySettings_UnsetSettingIsSkippedNotWrittenEmpty for coreUrl: a build that
// somehow has no value for its own address (the chart didn't set BOOTH_CORE_URL) skips the
// setting rather than writing an empty string, the same as every other unset shared setting.
func TestMergeIdentitySettings_CoreURLSkippedWhenCoreHasNoValue(t *testing.T) {
	identity := fullIdentity
	identity.CoreURL = ""

	c := fixtureChart(`{"issuerUrl":"oidc.issuerUrl","coreUrl":"core.url"}`)
	got, merged := mergeIdentitySettings(c, identity, map[string]any{})

	if _, present := got["core"]; present {
		t.Errorf("core = %v, want the key absent entirely, never an empty string", got["core"])
	}
	for _, name := range merged {
		if name == "coreUrl" {
			t.Error("merged reports coreUrl as supplied, but core has no value for it")
		}
	}
}

func TestMergeIdentitySettings_NoAnnotationMeansNoChange(t *testing.T) {
	c := fixtureChart("") // no booth.projectbooth.io/identity-keys annotation at all
	values := map[string]any{"replicaCount": float64(2)}

	got, merged := mergeIdentitySettings(c, fullIdentity, values)

	if len(got) != 1 || got["replicaCount"] != float64(2) {
		t.Errorf("values = %v, want it returned completely unchanged", got)
	}
	if merged != nil {
		t.Errorf("merged = %v, want nil", merged)
	}
}

func TestMergeIdentitySettings_InvalidJSONIsIgnoredAndLogged(t *testing.T) {
	c := fixtureChart(`{not valid json`)
	values := map[string]any{"replicaCount": float64(2)}

	var got map[string]any
	var merged []string
	logged := captureLog(t, func() {
		got, merged = mergeIdentitySettings(c, fullIdentity, values)
	})

	if len(got) != 1 || got["replicaCount"] != float64(2) {
		t.Errorf("values = %v, want it returned unchanged when the annotation can't be parsed", got)
	}
	if merged != nil {
		t.Errorf("merged = %v, want nil", merged)
	}
	if logged == "" {
		t.Error("expected a log line reporting the invalid annotation, got none")
	}
}

func TestMergeIdentitySettings_UnknownKeyIsIgnoredAndLoggedButOthersStillMerge(t *testing.T) {
	c := fixtureChart(`{"issuerUrl":"oidc.issuerUrl","bogusSetting":"oidc.bogus"}`)

	var got map[string]any
	var merged []string
	logged := captureLog(t, func() {
		got, merged = mergeIdentitySettings(c, fullIdentity, map[string]any{})
	})

	oidc := got["oidc"].(map[string]any)
	if _, present := oidc["bogus"]; present {
		t.Errorf("oidc.bogus = %v, want the unknown setting's path never written", oidc["bogus"])
	}
	if oidc["issuerUrl"] != fullIdentity.IssuerURL {
		t.Errorf("oidc.issuerUrl = %v, want the valid setting in the same annotation still merged", oidc["issuerUrl"])
	}
	if len(merged) != 1 || merged[0] != "issuerUrl" {
		t.Errorf("merged = %v, want exactly [issuerUrl]", merged)
	}
	if logged == "" {
		t.Error("expected a log line reporting the unknown setting name, got none")
	}
}

// TestMergeIdentitySettings_AppliesIdenticallyRegardlessOfInstallOrUpgrade exercises ADR 0113
// item 10 ("the same merge applies on upgrade and re-install, not only the first install") at
// the level this package's tests can actually reach without a real cluster (see
// manager_test.go's note: the Helm install/upgrade actions themselves need a real Kubernetes API
// server and are exercised by the integration layer, not here). Manager.Install calls
// mergeIdentitySettings exactly once, before branching on releaseExists, so the branch can never
// see a different merge — this proves the merge step itself is deterministic and has no
// knowledge of, or special case for, which branch follows, which is what makes that guarantee
// true by construction rather than by each branch remembering to call it.
func TestMergeIdentitySettings_AppliesIdenticallyRegardlessOfInstallOrUpgrade(t *testing.T) {
	c := fixtureChart(fullMapping)

	installValues, installMerged := mergeIdentitySettings(c, fullIdentity, map[string]any{})
	upgradeValues, upgradeMerged := mergeIdentitySettings(c, fullIdentity, map[string]any{})

	if len(installMerged) != len(upgradeMerged) {
		t.Fatalf("merged settings differ between calls: install=%v upgrade=%v", installMerged, upgradeMerged)
	}
	for i := range installMerged {
		if installMerged[i] != upgradeMerged[i] {
			t.Fatalf("merged settings differ between calls: install=%v upgrade=%v", installMerged, upgradeMerged)
		}
	}
	installOidc := installValues["oidc"].(map[string]any)
	upgradeOidc := upgradeValues["oidc"].(map[string]any)
	for _, key := range []string{"issuerUrl", "clientId", "jwksUrl", "workloadIssuerUrl", "groupsClaim"} {
		if installOidc[key] != upgradeOidc[key] {
			t.Errorf("oidc.%s differs between calls: install=%v upgrade=%v", key, installOidc[key], upgradeOidc[key])
		}
	}
}
