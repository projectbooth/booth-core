package lifecycle

import "testing"

// Test cases ported from booth-module-store's internal/catalog/chartref_test.go,
// since this file's ParseChartRef was lifted from that repo's ParseRegistryChartRef
// (ADR 0028) — behavior must stay identical.

func TestParseChartRef_OCI(t *testing.T) {
	got, err := ParseChartRef("oci://registry.example.com/charts/acme-forecast", "1.4.2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ChartRef{
		RepoURL:   "oci://registry.example.com/charts",
		ChartName: "acme-forecast",
		Version:   "1.4.2",
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseChartRef_TrailingSlash(t *testing.T) {
	got, err := ParseChartRef("oci://registry.example.com/charts/acme-forecast/", "1.4.2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ChartName != "acme-forecast" {
		t.Errorf("ChartName = %q, want acme-forecast", got.ChartName)
	}
}

func TestParseChartRef_Rejects(t *testing.T) {
	cases := []string{
		"",
		"https://charts.example.com/acme-forecast",
		"oci://acme-forecast",
		"oci://host/",
	}
	for _, c := range cases {
		if _, err := ParseChartRef(c, "1.0.0"); err == nil {
			t.Errorf("ParseChartRef(%q): expected error, got nil", c)
		}
	}
}

func TestChartRef_IsZero(t *testing.T) {
	if !(ChartRef{}).IsZero() {
		t.Error("empty ChartRef should be zero")
	}
	if (ChartRef{Path: "/some/path"}).IsZero() {
		t.Error("ChartRef with Path set should not be zero")
	}
	if (ChartRef{ChartName: "x"}).IsZero() {
		t.Error("ChartRef with ChartName set should not be zero")
	}
}
