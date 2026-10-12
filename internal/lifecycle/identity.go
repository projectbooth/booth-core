package lifecycle

import (
	"encoding/json"
	"log"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
)

// identityKeysAnnotation is the fleet's annotation domain (ADR 0113, contracts/module-manifest.md
// "Chart annotation: identity keys") — not "booth.dev/...", which nothing else in the fleet uses.
const identityKeysAnnotation = "booth.projectbooth.io/identity-keys"

// IdentitySettings is the platform's own shared identity configuration (ADR 0113), derived once
// from booth-core's resolved config.Config (cfg.OIDC.*, cfg.Workload.IssuerURL,
// cfg.IframeIdentity.IssuerURL) by the caller (cmd/core), not by this package. A field left empty
// means core has no value for that setting today (e.g. WorkloadIssuerURL when workload identity
// is disabled) — mergeIdentitySettings skips it rather than writing an empty string.
type IdentitySettings struct {
	IssuerURL         string
	ClientID          string
	JWKSURL           string
	WorkloadIssuerURL string
	GroupsClaim       string
	IframeIssuerURL   string
}

// sharedSettingNames are the only names contracts/module-manifest.md's annotation recognizes.
// Kept distinct from IdentitySettings.asMap() so "this build has no value for a real setting"
// (e.g. the workload issuer with workload identity off) is never confused with "this isn't a
// real setting at all" — the two get different handling and different log lines below.
var sharedSettingNames = map[string]bool{
	"issuerUrl":         true,
	"clientId":          true,
	"jwksUrl":           true,
	"workloadIssuerUrl": true,
	"groupsClaim":       true,
	"iframeIssuerUrl":   true,
}

// asMap exposes the shared setting names exactly as contracts/module-manifest.md's annotation
// keys spell them, omitting any field this deployment has no value for.
func (s IdentitySettings) asMap() map[string]string {
	m := map[string]string{}
	add := func(key, value string) {
		if value != "" {
			m[key] = value
		}
	}
	add("issuerUrl", s.IssuerURL)
	add("clientId", s.ClientID)
	add("jwksUrl", s.JWKSURL)
	add("workloadIssuerUrl", s.WorkloadIssuerURL)
	add("groupsClaim", s.GroupsClaim)
	add("iframeIssuerUrl", s.IframeIssuerURL)
	return m
}

// mergeIdentitySettings reads c's booth.projectbooth.io/identity-keys annotation, if any, and
// fills values with identity's settings at the annotation's declared dotted values paths —
// per leaf, not per block: a mapped path is set only when that exact leaf is absent from values
// already, so a caller who sets one nested key (e.g. oidc.clientId) still gets the others filled
// in (ADR 0113 point 8). A setting identity has no value for is skipped, never written as "".
//
// A chart with no annotation, an annotation that isn't valid JSON, or an annotation naming a key
// this build doesn't recognize is left alone for that key — logged, never failed. This merge
// exists to remove hand-typed values, not to become a new way an install can break.
//
// Returns the (possibly unchanged) values and the shared setting names actually merged in, sorted,
// for the caller to log/report back (ADR 0113's "the user should be able to see which values core
// supplied").
func mergeIdentitySettings(c *chart.Chart, identity IdentitySettings, values map[string]any) (map[string]any, []string) {
	raw, ok := c.Metadata.Annotations[identityKeysAnnotation]
	if !ok || raw == "" {
		return values, nil
	}

	var mapping map[string]string
	if err := json.Unmarshal([]byte(raw), &mapping); err != nil {
		log.Printf("lifecycle: chart %s's %s annotation is not valid JSON, ignoring it: %v", c.Name(), identityKeysAnnotation, err)
		return values, nil
	}

	available := identity.asMap()
	if values == nil {
		values = map[string]any{}
	}

	var merged []string
	for name, path := range mapping {
		if !sharedSettingNames[name] {
			log.Printf("lifecycle: chart %s's %s annotation names %q, which is not a shared identity setting; ignoring it", c.Name(), identityKeysAnnotation, name)
			continue
		}
		value, haveValue := available[name]
		if !haveValue {
			// A real setting, just not one this deployment has a value for (e.g. the
			// workload issuer with workload identity off) -- not an error, nothing to log.
			continue
		}
		if hasLeaf(values, path) {
			continue
		}
		setLeaf(values, path, value)
		merged = append(merged, name)
	}
	sort.Strings(merged)
	return values, merged
}

// hasLeaf reports whether dotted path already has a value in values. A non-map value sitting
// where the path expects to descend further is treated as present, not overwritten — the
// caller put something there, and this merge only ever fills gaps.
func hasLeaf(values map[string]any, path string) bool {
	segs := strings.Split(path, ".")
	cur := values
	for i, seg := range segs {
		v, ok := cur[seg]
		if !ok {
			return false
		}
		if i == len(segs)-1 {
			return true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return true
		}
		cur = next
	}
	return false
}

// setLeaf sets dotted path to value inside values, creating intermediate maps as needed.
func setLeaf(values map[string]any, path string, value string) {
	segs := strings.Split(path, ".")
	cur := values
	for i, seg := range segs {
		if i == len(segs)-1 {
			cur[seg] = value
			return
		}
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
}
