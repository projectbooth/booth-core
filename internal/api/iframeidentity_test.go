package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/projectbooth/booth-core/internal/directory"
	"github.com/projectbooth/booth-core/internal/iframeidentity"
	"github.com/projectbooth/booth-core/internal/registry"
	"github.com/projectbooth/booth-core/internal/workload"
)

func newIframeIdentityService(t *testing.T, issuer string) *iframeidentity.Service {
	t.Helper()
	keys, err := iframeidentity.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	return iframeidentity.NewService(keys, iframeidentity.Options{Issuer: issuer, GroupsClaim: "groups"})
}

func TestIframeIdentityWellKnownDocuments(t *testing.T) {
	idp := newTestIDP(t)
	svc := newIframeIdentityService(t, "http://core.example:8080"+iframeidentity.RoutePrefix)
	router := routerWith(t, idp, func(d *Deps) { d.IframeIdentity = svc })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, iframeidentity.RoutePrefix+iframeidentity.JWKSPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("JWKS status = %d", rec.Code)
	}
	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&jwks); err != nil || len(jwks.Keys) != 1 {
		t.Fatalf("JWKS = %+v, %v", jwks, err)
	}
	k := jwks.Keys[0]
	if k["kty"] != "RSA" || k["alg"] != "RS256" || k["use"] != "sig" {
		t.Errorf("unexpected key: %v", k)
	}
	if _, private := k["d"]; private {
		t.Error("JWKS exposes a private key")
	}

	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, iframeidentity.RoutePrefix+iframeidentity.DiscoveryPath, nil))
	var disc map[string]any
	_ = json.NewDecoder(rec2.Body).Decode(&disc)
	if disc["issuer"] != svc.Issuer() || disc["jwks_uri"] != svc.Issuer()+iframeidentity.JWKSPath {
		t.Errorf("discovery = %v", disc)
	}
}

func TestRouter_WithoutIframeIdentityExposesNothing(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, nil) // Deps.IframeIdentity left nil

	for _, path := range []string{iframeidentity.RoutePrefix + iframeidentity.JWKSPath, iframeidentity.RoutePrefix + iframeidentity.DiscoveryPath} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200 with the iframe-identity issuer off", path)
		}
	}
}

// The iframe-identity and workload-token issuers must never be reachable at the same URL or
// share key material — trusting one issuer class must not implicitly trust the other, since a
// workload token is defined as never a person while an iframe-identity assertion always is.
func TestIframeIdentityAndWorkloadIssuersAreIndependent(t *testing.T) {
	idp := newTestIDP(t)
	base := "http://core.example:8080"
	workloadKeys, err := workload.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	workloadSvc := workload.NewService(workloadKeys, registry.New(), directory.NewMemoryStore(), workload.Options{Issuer: base, Audience: "c", GroupsClaim: "groups"})
	iframeSvc := newIframeIdentityService(t, base+iframeidentity.RoutePrefix)

	router := routerWith(t, idp, func(d *Deps) { d.Workload = workloadSvc; d.IframeIdentity = iframeSvc })

	if workloadSvc.Issuer() == iframeSvc.Issuer() {
		t.Fatalf("both issuers resolved to the same URL: %s", workloadSvc.Issuer())
	}

	getJWKS := func(path string) map[string]any {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		var doc map[string]any
		_ = json.NewDecoder(rec.Body).Decode(&doc)
		return doc
	}
	workloadJWKS := getJWKS(workload.JWKSPath)
	iframeJWKS := getJWKS(iframeidentity.RoutePrefix + iframeidentity.JWKSPath)

	wKeys, _ := workloadJWKS["keys"].([]any)
	iKeys, _ := iframeJWKS["keys"].([]any)
	if len(wKeys) != 1 || len(iKeys) != 1 {
		t.Fatalf("expected exactly one key each: workload=%v iframe=%v", wKeys, iKeys)
	}
	wKid := wKeys[0].(map[string]any)["kid"]
	iKid := iKeys[0].(map[string]any)["kid"]
	if wKid == iKid {
		t.Fatalf("workload and iframe-identity issuers published the same key id %v", wKid)
	}
}
