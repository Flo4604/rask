package gotool

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDecodeCurrentModuleSelectsLongestDirPrefix protects go.work module selection.
func TestDecodeCurrentModuleSelectsLongestDirPrefix(t *testing.T) {
	output := `{"Path":"example.com/root","Dir":"/repo"}` + "\n" +
		`{"Path":"example.com/root/app","Dir":"/repo/app"}` + "\n" +
		`{"Path":"example.com/other","Dir":"/other"}` + "\n"
	module, err := decodeCurrentModule([]byte(output), "/repo/app/internal/pkg")
	if err != nil {
		t.Fatalf("decode module: %v", err)
	}
	if module.Path != "example.com/root/app" || module.Dir != "/repo/app" {
		t.Fatalf("module = %+v, want deepest containing module", module)
	}
}

// TestSaltEnvironmentIgnoresGOGCCFLAGS protects stable fingerprint salting.
func TestSaltEnvironmentIgnoresGOGCCFLAGS(t *testing.T) {
	first := "GOARCH='arm64'\n" +
		"GOGCCFLAGS='-ffile-prefix-map=/tmp/go-build111=/tmp/go-build'\n" +
		"GOFLAGS='-race'\n"
	second := "GOARCH='arm64'\n" +
		"GOGCCFLAGS='-ffile-prefix-map=/tmp/go-build222=/tmp/go-build'\n" +
		"GOFLAGS='-race'\n"
	if string(SaltEnvironment([]byte(first))) != string(SaltEnvironment([]byte(second))) {
		t.Fatalf("GOGCCFLAGS-only changes must not affect salt environment")
	}
}

// TestSaltEnvironmentPreservesGOFLAGS protects user flag invalidation.
func TestSaltEnvironmentPreservesGOFLAGS(t *testing.T) {
	first := "GOARCH='arm64'\nGOFLAGS='-race'\n"
	second := "GOARCH='arm64'\nGOFLAGS='-count=1'\n"
	if string(SaltEnvironment([]byte(first))) == string(SaltEnvironment([]byte(second))) {
		t.Fatalf("GOFLAGS changes must affect salt environment")
	}
}

// TestParseListedTestsFiltersSummaries protects `go test -list` stdout parsing.
func TestParseListedTestsFiltersSummaries(t *testing.T) {
	output := []byte("TestAlpha\nTestBeta\nok  \texample.com/m/pkg\t0.013s\n" +
		"FAIL\texample.com/m/bad\t0.001s\n?\texample.com/m/none\t[no test files]\n")
	tests := parseListedTests(output)
	if strings.Join(tests, ",") != "TestAlpha,TestBeta" {
		t.Fatalf("tests = %q, want listed names only", tests)
	}
}

// TestGoTestArgumentsAlwaysShuffle protects rask's fixed suite invocation.
func TestGoTestArgumentsAlwaysShuffle(t *testing.T) {
	arguments := goTestArguments("example.com/m/pkg")
	for _, want := range []string{"test", "-json", "-count=1", "-failfast", "-shuffle=on", "example.com/m/pkg"} {
		if !slices.Contains(arguments, want) {
			t.Fatalf("arguments = %v, missing %s", arguments, want)
		}
	}
	listArguments := goListTestArguments("example.com/m/pkg")
	if slices.Contains(listArguments, "-shuffle=on") {
		t.Fatalf("list arguments = %v, want no -shuffle for name listing", listArguments)
	}
}

// TestSystemToolListTestsFailureReturnsError protects lister command failures.
func TestSystemToolListTestsFailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.26\n")
	writeTestFile(t, filepath.Join(dir, "bad"), "bad.go", "package bad\nfunc Broken() {\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := SystemTool{}.ListTests(ctx, dir, "example.com/m/bad")
	if err == nil || !strings.Contains(err.Error(), "go test -list failed") {
		t.Fatalf("err = %v, want go test -list failure", err)
	}
}

// writeTestFile writes a file in a temporary module fixture.
func writeTestFile(t *testing.T, dir string, name string, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
