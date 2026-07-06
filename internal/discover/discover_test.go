package discover

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chronark/rask/internal/gotool"
)

// TestDiscoverListsWholeModuleOnceForDefaultPattern protects the ./... single-list optimization.
func TestDiscoverListsWholeModuleOnceForDefaultPattern(t *testing.T) {
	module := moduleFixtureRoot(t)
	tool := &fakeTool{all: []gotool.Package{packageFixture(module, "app")}}
	packages, err := discoverForTest(tool, module, module.Dir, []string{"./..."})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if tool.calls["./..."] != 1 {
		t.Fatalf("go list ./... calls = %d, want 1", tool.calls["./..."])
	}
	if len(packages) != 1 || !packages[0].Selected {
		t.Fatalf("packages = %+v, want one selected package", packages)
	}
}

// TestDiscoverMarksSelectedWithinFullClosure protects pattern selection after full-closure listing.
func TestDiscoverMarksSelectedWithinFullClosure(t *testing.T) {
	module := moduleFixtureRoot(t)
	app := packageFixture(module, "app")
	app.Imports = []string{module.Path + "/lib"}
	app.Selected = false
	lib := packageFixture(module, "lib")
	tool := &fakeTool{selected: []gotool.Package{app}, all: []gotool.Package{lib, app}}
	packages, err := discoverForTest(tool, module, module.Dir, []string{"./app"})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	byPath := packagesByPath(packages)
	if byPath[module.Path+"/lib"].Selected {
		t.Fatalf("dependency package must not be selected")
	}
	if !byPath[module.Path+"/app"].Selected {
		t.Fatalf("pattern package must be selected")
	}
}

// TestDiscoverRejectsLoadAndSyntaxErrors protects go-list Error handling and local parse guards.
func TestDiscoverRejectsLoadAndSyntaxErrors(t *testing.T) {
	module := moduleFixtureRoot(t)
	broken := packageFixture(module, "broken")
	broken.Error = &gotool.PackageError{Err: "expected '}', found EOF"}
	_, err := discoverForTest(&fakeTool{all: []gotool.Package{broken}}, module, module.Dir, []string{"./..."})
	if err == nil || !strings.Contains(err.Error(), module.Path+"/broken") {
		t.Fatalf("err = %v, want package load error", err)
	}
	writeFile(t, module.Dir, "bad.go", "package bad\nfunc x() {")
	bad := gotool.Package{Dir: module.Dir, ImportPath: module.Path, GoFiles: []string{"bad.go"}}
	_, err = discoverForTest(&fakeTool{all: []gotool.Package{bad}}, module, module.Dir, []string{"./..."})
	if err == nil || !strings.Contains(err.Error(), "package "+module.Path+" is broken") {
		t.Fatalf("err = %v, want syntax error", err)
	}
}

// TestDiscoverRejectsEmptyAndOutsideSelections protects zero-suite success cases.
func TestDiscoverRejectsEmptyAndOutsideSelections(t *testing.T) {
	module := moduleFixtureRoot(t)
	_, err := discoverForTest(&fakeTool{}, module, module.Dir, []string{"./missing"})
	if err == nil || !strings.Contains(err.Error(), "matched no packages") {
		t.Fatalf("err = %v, want empty pattern error", err)
	}
	outside := gotool.Package{
		Dir: filepath.Join(t.TempDir(), "other"), ImportPath: "example.com/other/pkg", Name: "pkg",
		GoFiles: []string{"pkg.go"}, Selected: true,
	}
	tool := &fakeTool{selected: []gotool.Package{outside}, all: []gotool.Package{packageFixture(module, "app")}}
	_, err = discoverForTest(tool, module, module.Dir, []string{"../other"})
	if err == nil || !strings.Contains(err.Error(), "outside module "+module.Path) {
		t.Fatalf("err = %v, want outside module error", err)
	}
}

// TestDiscoverListsPatternsFromUserDir protects relative pattern resolution.
func TestDiscoverListsPatternsFromUserDir(t *testing.T) {
	module := moduleFixtureRoot(t)
	userDir := filepath.Join(module.Dir, "worker")
	app := packageFixture(module, "worker/api")
	tool := &fakeTool{selected: []gotool.Package{app}, all: []gotool.Package{app}}
	if _, err := discoverForTest(tool, module, userDir, []string{"./api"}); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got := tool.dirs["./api"]; got != userDir {
		t.Fatalf("pattern dir = %q, want %q", got, userDir)
	}
	if got := tool.dirs["./..."]; got != module.Dir {
		t.Fatalf("closure dir = %q, want %q", got, module.Dir)
	}
}

// TestDiscoverMergesLocalReplacePackagesUnselected protects local dependency hashing inputs.
func TestDiscoverMergesLocalReplacePackagesUnselected(t *testing.T) {
	module := moduleFixtureRootWithGoMod(t, "module example.com/m\n\ngo 1.26\n\nreplace example.com/dep => ../dep\n")
	dep := moduleFixtureRootAt(t, filepath.Join(filepath.Dir(module.Dir), "dep"), "example.com/dep")
	app := packageFixture(module, "app")
	utility := packageFixture(dep, "util")
	tool := &fakeTool{allByDir: map[string][]gotool.Package{
		module.Dir: {app},
		dep.Dir:    {utility},
	}}
	packages, err := discoverForTest(tool, module, module.Dir, []string{"./..."})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	byPath := packagesByPath(packages)
	if !byPath[app.ImportPath].Selected {
		t.Fatalf("main package must stay selected")
	}
	if byPath[utility.ImportPath].Selected {
		t.Fatalf("local replace package must not be selected")
	}
}

// TestDiscovererShufflesEmissionOrder protects inter-suite package randomization.
func TestDiscovererShufflesEmissionOrder(t *testing.T) {
	module := moduleFixtureRoot(t)
	packages := []gotool.Package{
		packageFixture(module, "a"), packageFixture(module, "b"), packageFixture(module, "c"),
		packageFixture(module, "d"), packageFixture(module, "e"), packageFixture(module, "f"),
	}
	first := discoverOrderForTest(t, module, packages, rand.New(rand.NewPCG(1, 101)))
	second := discoverOrderForTest(t, module, packages, rand.New(rand.NewPCG(2, 102)))
	if strings.Join(first, ",") == strings.Join(second, ",") {
		t.Fatalf("orders matched: %v", first)
	}
	if sameSet(first, second) != nil {
		t.Fatalf("orders contain different packages: %v vs %v", first, second)
	}
}

// TestDiscoverRejectsBrokenAdditionalRoot protects clear local-root failures.
func TestDiscoverRejectsBrokenAdditionalRoot(t *testing.T) {
	module := moduleFixtureRootWithGoMod(t, "module example.com/m\n\ngo 1.26\n\nreplace example.com/dep => ../missing\n")
	app := packageFixture(module, "app")
	_, err := discoverForTest(&fakeTool{all: []gotool.Package{app}}, module, module.Dir, []string{"./..."})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v, want missing root named", err)
	}
}

// TestDiscoverRejectsAdditionalRootListFailure protects go-list errors in local roots.
func TestDiscoverRejectsAdditionalRootListFailure(t *testing.T) {
	module := moduleFixtureRootWithGoMod(t, "module example.com/m\n\ngo 1.26\n\nreplace example.com/dep => ../dep\n")
	dep := moduleFixtureRootAt(t, filepath.Join(filepath.Dir(module.Dir), "dep"), "example.com/dep")
	app := packageFixture(module, "app")
	tool := &fakeTool{
		allByDir:     map[string][]gotool.Package{module.Dir: {app}},
		listErrByDir: map[string]error{dep.Dir: fmt.Errorf("dep broken")},
	}
	_, err := discoverForTest(tool, module, module.Dir, []string{"./..."})
	if err == nil || !strings.Contains(err.Error(), dep.Dir) {
		t.Fatalf("err = %v, want listed root named", err)
	}
}

// TestLocalReplaceRootsParsesSupportedForms protects the minimal go.mod scan.
func TestLocalReplaceRootsParsesSupportedForms(t *testing.T) {
	module := moduleFixtureRootWithGoMod(t, `module example.com/m

go 1.26

replace example.com/single => ../single
replace example.com/version => example.com/version v1.2.3
replace (
	example.com/block => ./block
	example.com/quoted => "../quoted"
)
`)
	roots, err := LocalReplaceRoots(module.Dir)
	if err != nil {
		t.Fatalf("local replacements: %v", err)
	}
	want := []string{
		filepath.Clean(filepath.Join(module.Dir, "../single")),
		filepath.Clean(filepath.Join(module.Dir, "block")),
		filepath.Clean(filepath.Join(module.Dir, "../quoted")),
	}
	if fmt.Sprint(roots) != fmt.Sprint(want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}
}

// moduleFixtureRoot creates a minimal module directory for discovery tests.
func moduleFixtureRoot(t *testing.T) gotool.Module {
	t.Helper()
	return moduleFixtureRootWithGoMod(t, "module example.com/m\n\ngo 1.26\n")
}

// moduleFixtureRootWithGoMod creates a module fixture with custom go.mod content.
func moduleFixtureRootWithGoMod(t *testing.T, goMod string) gotool.Module {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", goMod)
	return gotool.Module{Path: "example.com/m", Dir: dir}
}

// moduleFixtureRootAt creates a minimal module directory at dir.
func moduleFixtureRootAt(t *testing.T, dir string, modulePath string) gotool.Module {
	t.Helper()
	writeFile(t, dir, "go.mod", "module "+modulePath+"\n\ngo 1.26\n")
	return gotool.Module{Path: modulePath, Dir: dir}
}

// packageFixture returns a selected package fixture.
func packageFixture(module gotool.Module, name string) gotool.Package {
	dir := filepath.Join(module.Dir, name)
	return gotool.Package{
		Dir: dir, ImportPath: module.Path + "/" + name, Name: name,
		GoFiles: []string{name + ".go"}, TestGoFiles: []string{name + "_test.go"},
		Selected: true,
	}
}

// writeFile creates a test file and fails the test on filesystem errors.
func writeFile(t *testing.T, dir string, name string, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// packagesByPath indexes package fixtures by import path.
func packagesByPath(packages []gotool.Package) map[string]gotool.Package {
	byPath := make(map[string]gotool.Package, len(packages))
	for _, p := range packages {
		byPath[p.ImportPath] = p
	}
	return byPath
}

// discoverForTest keeps unit tests focused on discovery without channel emission.
func discoverForTest(
	tool gotool.Tool,
	module gotool.Module,
	patternDir string,
	patterns []string,
) ([]gotool.Package, error) {
	discoverer := &Discoverer{
		tool:       tool,
		module:     module,
		patternDir: patternDir,
		patterns:   patterns,
	}
	return discoverer.discover()
}

// discoverOrderForTest returns package emission order from a seeded discoverer.
func discoverOrderForTest(
	t *testing.T,
	module gotool.Module,
	packages []gotool.Package,
	rand *rand.Rand,
) []string {
	t.Helper()
	out := make(chan gotool.Package, len(packages))
	tool := &fakeTool{all: append([]gotool.Package(nil), packages...)}
	discoverer := newWithRand(tool, module, module.Dir, []string{"./..."}, out, rand)
	if err := discoverer.Run(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	order := make([]string, 0, len(packages))
	for p := range out {
		order = append(order, p.ImportPath)
	}
	return order
}

// sameSet reports a missing item when two string slices differ as sets.
func sameSet(first []string, second []string) error {
	counts := make(map[string]int, len(first))
	for _, item := range first {
		counts[item]++
	}
	for _, item := range second {
		counts[item]--
	}
	for item, count := range counts {
		if count != 0 {
			return fmt.Errorf("%s count delta %d", item, count)
		}
	}
	return nil
}

// fakeTool records package-listing calls for discovery tests.
type fakeTool struct {
	selected     []gotool.Package
	all          []gotool.Package
	allByDir     map[string][]gotool.Package
	listErrByDir map[string]error
	calls        map[string]int
	dirs         map[string]string
}

// CurrentModule fails because discovery tests receive modules directly.
func (f *fakeTool) CurrentModule() (gotool.Module, error) {
	return gotool.Module{}, fmt.Errorf("unexpected CurrentModule call")
}

// ListPackages returns selected packages for narrow patterns and all packages for ./....
func (f *fakeTool) ListPackages(dir string, patterns []string) ([]gotool.Package, error) {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	if f.dirs == nil {
		f.dirs = make(map[string]string)
	}
	key := strings.Join(patterns, " ")
	f.calls[key]++
	f.dirs[key] = dir
	if len(patterns) == 1 && patterns[0] == "./..." {
		if err := f.listErrByDir[dir]; err != nil {
			return nil, err
		}
		if f.allByDir != nil {
			return f.allByDir[dir], nil
		}
		return f.all, nil
	}
	return f.selected, nil
}

// ListModules fails because discovery tests receive modules directly.
func (f *fakeTool) ListModules(string) ([]gotool.Module, error) {
	return nil, fmt.Errorf("unexpected ListModules call")
}

// Environment returns a stable fake Go environment for local-root enumeration.
func (f *fakeTool) Environment(string) ([]byte, error) {
	return []byte("GOARCH='amd64'\nGOOS='linux'\nGOVERSION='go1.26'\nGOWORK=''\n"), nil
}

// ListTests fails because discovery never enumerates tests.
func (f *fakeTool) ListTests(context.Context, string, string) ([]string, error) {
	return nil, fmt.Errorf("unexpected ListTests call")
}

// StartTests fails because discovery never starts test processes.
func (f *fakeTool) StartTests(context.Context, string, string) (gotool.TestProcess, error) {
	return nil, fmt.Errorf("unexpected StartTests call")
}
