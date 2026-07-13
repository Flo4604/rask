package frontend

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestRenderWhySuite protects the traced-suite block layout and alignment.
func TestRenderWhySuite(t *testing.T) {
	out := new(bytes.Buffer)
	frontend := New(out, new(bytes.Buffer), false, nil)
	frontend.RenderWhy(WhyReport{ModulePath: "example.com/m", Suites: []WhySuite{{
		ImportPath:    "example.com/m/dependent",
		HasTests:      true,
		CacheKey:      strings.Repeat("a", 64),
		Cached:        true,
		CachedPassed:  12,
		CachedIgnored: 1,
		CachedAt:      time.Now().Add(-34 * time.Minute),
		SaltFiles:     []string{"/m/go.mod", "/m/go.sum"},
		TestFiles:     []string{"dependent_test.go", "testdata/fixture.txt"},
		PackageDigest: strings.Repeat("b", 64),
		PackageFiles:  []string{"dependent.go"},
		Dependencies: []WhyDependency{
			{ImportPath: "example.com/m/dependency", Digest: strings.Repeat("c", 64), Depth: 0},
			{ImportPath: "example.com/m/leaf", Digest: strings.Repeat("d", 64), Depth: 1},
		},
		TestOnlyImports: []WhyDependency{
			{ImportPath: "example.com/m/leaf", Digest: strings.Repeat("d", 64), Depth: 0, Repeated: true},
		},
	}}})
	rendered := out.String()
	for _, want := range []string{
		"Suite example.com/m/dependent\n\n  status",
		"  status     cached — 12 passed; 1 ignored, stored 34m ago",
		"  cache key  " + strings.Repeat("a", 64),
		"  module inputs      hashed into every suite's cache key",
		"    go env\n    /m/go.mod\n    /m/go.sum",
		"  test files         hashed into this suite only",
		"    testdata/fixture.txt",
		"  package files      build digest " + strings.Repeat("b", 12),
		"  dependencies       build digests of transitive imports",
		"    └─ dependency    " + strings.Repeat("c", 12),
		"       └─ leaf       " + strings.Repeat("d", 12),
		"  test-only imports  imported by test files only",
		"    └─ leaf          shown above",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("output missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "Traced") {
		t.Fatalf("single-suite report must not summarize:\n%s", rendered)
	}
}

// TestRenderWhyStaleAndTestless protects the stale verdict, testless block,
// and the multi-suite summary.
func TestRenderWhyStaleAndTestless(t *testing.T) {
	out := new(bytes.Buffer)
	frontend := New(out, new(bytes.Buffer), false, nil)
	frontend.RenderWhy(WhyReport{ModulePath: "example.com/m", Suites: []WhySuite{
		{ImportPath: "example.com/m/dependent", HasTests: true, CacheKey: strings.Repeat("a", 64)},
		{ImportPath: "example.com/m/helper"},
	}})
	rendered := out.String()
	for _, want := range []string{
		"  status     stale — no cached result for this key; next run executes go test",
		"\n\nSuite example.com/m/helper\n\n  no test files — nothing to run or cache",
		"Traced 2 suites: 1 stale, 1 without tests",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("output missing %q:\n%s", want, rendered)
		}
	}
}

// TestWhyAge protects rerun-decision precision at each magnitude.
func TestWhyAge(t *testing.T) {
	for _, tc := range []struct {
		elapsed time.Duration
		want    string
	}{
		{30 * time.Second, "moments"},
		{34 * time.Minute, "34m"},
		{5 * time.Hour, "5h"},
		{49 * time.Hour, "2d"},
	} {
		if got := whyAge(tc.elapsed); got != tc.want {
			t.Fatalf("whyAge(%s) = %q, want %q", tc.elapsed, got, tc.want)
		}
	}
}
