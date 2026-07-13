package fingerprint

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/chronark/rask/internal/gotool"
)

// TestExplainMatchesSuiteDigests protects trace-to-cache-key parity.
func TestExplainMatchesSuiteDigests(t *testing.T) {
	module, packages := fingerprintFixture(t)
	digests := fingerprintsForTest(t, module, packages, nil)
	explanation := explainForTest(t, module, packages)
	if len(explanation.Suites) == 0 {
		t.Fatalf("explanation has no suites")
	}
	for _, suite := range explanation.Suites {
		if digests[suite.ImportPath] != suite.Digest {
			t.Fatalf("suite %s traced digest %s, want cache key %s",
				suite.ImportPath, suite.Digest, digests[suite.ImportPath])
		}
	}
	if len(explanation.Builds) != len(packages) {
		t.Fatalf("builds traced %d packages, want %d", len(explanation.Builds), len(packages))
	}
}

// TestExplainTracesDependencyEdges protects the reported graph and file inputs.
func TestExplainTracesDependencyEdges(t *testing.T) {
	module, packages := fingerprintFixture(t)
	writeFile(t, filepath.Join(module.Dir, "dependent", "testdata"), "fixture.txt", "fixture\n")
	explanation := explainForTest(t, module, packages)
	dependent := suiteTraceForTest(t, explanation, module.Path+"/dependent")
	if fmt.Sprint(dependent.TestOnlyImports) != fmt.Sprintf("[%s/helper]", module.Path) {
		t.Fatalf("test-only imports = %v, want helper", dependent.TestOnlyImports)
	}
	if fmt.Sprint(dependent.TestdataFiles) != "[testdata/fixture.txt]" {
		t.Fatalf("testdata files = %v, want fixture", dependent.TestdataFiles)
	}
	if fmt.Sprint(dependent.TestFiles) != "[dependent_test.go]" {
		t.Fatalf("test files = %v", dependent.TestFiles)
	}
	build := explanation.Builds[module.Path+"/dependent"]
	if fmt.Sprint(build.Imports) != fmt.Sprintf("[%s/dependency]", module.Path) {
		t.Fatalf("build imports = %v, want dependency", build.Imports)
	}
	if fmt.Sprint(build.Files) != "[dependent.go]" {
		t.Fatalf("build files = %v", build.Files)
	}
	if build.Digest == "" || explanation.Builds[module.Path+"/dependency"].Digest == "" {
		t.Fatalf("build digests missing: %+v", explanation.Builds)
	}
}

// TestExplainTracesSaltInputs protects the reported invalidation-root files.
func TestExplainTracesSaltInputs(t *testing.T) {
	module, packages := fingerprintFixture(t)
	explanation := explainForTest(t, module, packages)
	if len(explanation.Suites) != 3 {
		t.Fatalf("suites = %d, want dependency, dependent, and bystander", len(explanation.Suites))
	}
	if explanation.Salt.Digest == "" || explanation.Salt.GoWork != "" {
		t.Fatalf("salt = %+v, want digest without workspace", explanation.Salt)
	}
	if len(explanation.Salt.Roots) != 1 || explanation.Salt.Roots[0].GoSum {
		t.Fatalf("salt roots = %+v, want one sum-less root", explanation.Salt.Roots)
	}
	writeFile(t, module.Dir, "go.sum", "example.com/dep v1.0.0 h1:abc=\n")
	withSum := explainForTest(t, module, packages)
	if len(withSum.Salt.Roots) != 1 || !withSum.Salt.Roots[0].GoSum {
		t.Fatalf("salt roots = %+v, want hashed go.sum", withSum.Salt.Roots)
	}
	if withSum.Salt.Digest == explanation.Salt.Digest {
		t.Fatalf("salt digest must move when go.sum appears")
	}
}

// explainForTest traces the fixture universe and fails the test on errors.
func explainForTest(
	t *testing.T,
	module gotool.Module,
	packages []gotool.Package,
) Explanation {
	t.Helper()
	explanation, err := Explain(context.Background(), &fakeTool{}, module, packages)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	return explanation
}

// suiteTraceForTest finds one traced suite by import path.
func suiteTraceForTest(t *testing.T, explanation Explanation, importPath string) SuiteTrace {
	t.Helper()
	for _, suite := range explanation.Suites {
		if suite.ImportPath == importPath {
			return suite
		}
	}
	t.Fatalf("suite %s missing from explanation", importPath)
	return SuiteTrace{}
}
