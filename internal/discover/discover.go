// Package discover lists the package closure used by one rask run.
//
// Discovery sits on top of [gotool.Tool]. It resolves the user's patterns into
// selected packages, loads the local package closure needed for fingerprinting,
// and rejects load errors before hashing starts.
//
// [Discoverer.Run] is a lifecycle wrapper over the pure discovery operation.
// It starts no internal goroutines; concurrent use is safe when each run has
// its own package channel and the supplied [gotool.Tool] implementation is safe.
package discover

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/gotool"
)

// Discoverer lists packages and streams the validated closure to a channel.
//
// Discoverer is safe to run once. Run owns its package channel: it emits every
// discovered package with ctx-guarded backpressure and closes the channel before
// returning. Cancellation stops pending emits and returns the context error.
type Discoverer struct {
	tool       gotool.Tool
	module     gotool.Module
	patternDir string
	patterns   []string
	out        chan<- gotool.Package
	rand       *rand.Rand
}

// New returns a discoverer that emits package metadata to out.
func New(
	tool gotool.Tool,
	module gotool.Module,
	patternDir string,
	patterns []string,
	out chan<- gotool.Package,
) *Discoverer {
	assert.NotNil(tool, "discovery needs a go tool")
	assert.NotNil(out, "discovery needs a package channel")
	return newWithRand(tool, module, patternDir, patterns, out, randomRand())
}

// newWithRand returns a discoverer using rand for emission-order shuffling.
func newWithRand(
	tool gotool.Tool,
	module gotool.Module,
	patternDir string,
	patterns []string,
	out chan<- gotool.Package,
	rng *rand.Rand,
) *Discoverer {
	assert.NotNil(rng, "discovery needs a random source")
	return &Discoverer{
		tool: tool, module: module, patternDir: patternDir, patterns: patterns, out: out, rand: rng,
	}
}

// Run discovers packages, emits them to the package channel, and closes it.
//
// Run is the channel's only sender and closes it with defer before returning.
// Every send selects on ctx.Done so cancellation can unblock downstream
// backpressure without panicking or leaking a goroutine.
func (d *Discoverer) Run(ctx context.Context) error {
	defer close(d.out)
	packages, err := discover(d.tool, d.module, d.patternDir, d.patterns)
	if err != nil {
		return err
	}
	shufflePackages(packages, d.rand)
	for _, p := range packages {
		select {
		case d.out <- p:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// randomRand returns a fresh process-local pseudo-random source.
func randomRand() *rand.Rand {
	seed := uint64(time.Now().UnixNano())
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// shufflePackages permutes package emission order in place.
func shufflePackages(packages []gotool.Package, rng *rand.Rand) {
	rng.Shuffle(len(packages), func(i int, j int) {
		packages[i], packages[j] = packages[j], packages[i]
	})
}

// discover returns the full package closure with selected packages marked.
//
// When patterns are exactly `./...`, the selected list is already the full
// closure and rask makes a single [gotool.Tool.ListPackages] call. Narrower
// patterns are listed once for selection and then `./...` is listed for the
// dependency closure needed by fingerprinting.
func discover(
	tool gotool.Tool,
	module gotool.Module,
	patternDir string,
	patterns []string,
) ([]gotool.Package, error) {
	roots, err := LocalModuleRoots(tool, module)
	if err != nil {
		return nil, err
	}
	selected, err := tool.ListPackages(patternDir, patterns)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("package pattern %s matched no packages", strings.Join(patterns, " "))
	}
	all := selected
	if !wholeModulePattern(patterns) {
		all, err = tool.ListPackages(module.Dir, []string{"./..."})
		if err != nil {
			return nil, err
		}
	}
	all = markModuleDir(all, module.Dir)
	for _, root := range additionalRoots(roots, module.Dir) {
		extra, err := listAdditionalRoot(tool, root)
		if err != nil {
			return nil, err
		}
		all = append(all, extra...)
	}
	return markSelected(module, all, selected)
}

// LocalModuleRoots returns the sorted local module roots that affect fingerprints.
func LocalModuleRoots(tool gotool.Tool, module gotool.Module) ([]string, error) {
	environment, err := tool.Environment(module.Dir)
	if err != nil {
		return nil, err
	}
	return LocalModuleRootsFromEnvironment(tool, module, environment)
}

// LocalModuleRootsFromEnvironment returns local module roots using cached go-env bytes.
func LocalModuleRootsFromEnvironment(
	tool gotool.Tool,
	module gotool.Module,
	environment []byte,
) ([]string, error) {
	roots := newRootSet()
	if err := roots.add(module.Dir); err != nil {
		return nil, err
	}
	if GoWorkPath(environment) != "" {
		if err := addWorkspaceRoots(tool, module.Dir, roots); err != nil {
			return nil, err
		}
	}
	return addReplaceRoots(roots)
}

// GoWorkPath returns the active go.work path from full go-env output.
func GoWorkPath(environment []byte) string {
	value := goEnvValue(environment, "GOWORK")
	if value == "off" {
		return ""
	}
	return value
}

// LocalReplaceRoots returns filesystem replace targets from root's go.mod.
//
// This is a minimal line-scan parser for ordinary single-line and block-form
// replace directives. It strips line comments before tokenizing and does not
// attempt full modfile parsing of escaped whitespace or comments inside quotes.
func LocalReplaceRoots(root string) ([]string, error) {
	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("enumerating local replaces in %s: %w", root, err)
	}
	roots := make([]string, 0)
	inBlock := false
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(strings.Split(raw, "//")[0])
		if line == "replace (" {
			inBlock = true
			continue
		}
		if inBlock && line == ")" {
			inBlock = false
			continue
		}
		target, ok, err := localReplaceTarget(line, inBlock)
		if err != nil {
			return nil, fmt.Errorf("enumerating local replaces in %s: %w", root, err)
		}
		if ok {
			roots = append(roots, resolveReplaceRoot(root, target))
		}
	}
	return roots, nil
}

// goEnvValue extracts one KEY=value line from go env's shell-style output.
func goEnvValue(environment []byte, key string) string {
	prefix := key + "="
	for _, raw := range strings.Split(string(environment), "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return strings.Trim(strings.TrimPrefix(line, prefix), `'"`)
	}
	return ""
}

// rootSet stores canonical local module roots in insertion-independent order.
type rootSet struct {
	seen  map[string]bool
	roots []string
}

// newRootSet returns an empty local-module root set.
func newRootSet() *rootSet {
	return &rootSet{seen: make(map[string]bool)}
}

// add records dir after absolutizing and validating the root.
func (s *rootSet) add(dir string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving local module root %s: %w", dir, err)
	}
	root = filepath.Clean(root)
	assert.True(filepath.IsAbs(root), "canonical root %s must be absolute", root)
	if s.seen[root] {
		return nil
	}
	if err := validateModuleRoot(root); err != nil {
		return err
	}
	s.seen[root] = true
	s.roots = append(s.roots, root)
	return nil
}

// sorted returns the root set in stable path order.
func (s *rootSet) sorted() []string {
	roots := append([]string(nil), s.roots...)
	sort.Strings(roots)
	return roots
}

// addWorkspaceRoots adds every go.work module reported by go list -m -json.
func addWorkspaceRoots(tool gotool.Tool, moduleDir string, roots *rootSet) error {
	modules, err := tool.ListModules(moduleDir)
	if err != nil {
		return fmt.Errorf("enumerating workspace modules from %s: %w", moduleDir, err)
	}
	for _, module := range modules {
		if module.Dir == "" {
			return fmt.Errorf("workspace module %s has no directory", module.Path)
		}
		if err := roots.add(module.Dir); err != nil {
			return err
		}
	}
	return nil
}

// addReplaceRoots adds local filesystem replace targets from every known root.
//
// The loop follows replaces transitively: it re-reads len(roots.roots) each
// pass so roots appended mid-loop are scanned too. rootSet dedupes canonical
// paths, so the loop is bounded by the number of distinct local roots even
// when replaces form a cycle.
func addReplaceRoots(roots *rootSet) ([]string, error) {
	for index := 0; index < len(roots.roots); index++ {
		replacements, err := LocalReplaceRoots(roots.roots[index])
		if err != nil {
			return nil, err
		}
		for _, replacement := range replacements {
			if err := roots.add(replacement); err != nil {
				return nil, err
			}
		}
	}
	return roots.sorted(), nil
}

// validateModuleRoot checks that root is an existing Go module directory.
func validateModuleRoot(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("local module root %s is unavailable: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local module root %s is not a directory", root)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return fmt.Errorf("local module root %s lacks go.mod: %w", root, err)
	}
	return nil
}

// localReplaceTarget returns a filesystem replacement target from one go.mod line.
func localReplaceTarget(line string, inBlock bool) (string, bool, error) {
	if line == "" || (!inBlock && !strings.HasPrefix(line, "replace ")) {
		return "", false, nil
	}
	fields := strings.Fields(strings.TrimPrefix(line, "replace "))
	for index, field := range fields {
		if field != "=>" || index+1 >= len(fields) {
			continue
		}
		target, err := unquotePath(fields[index+1])
		if err != nil {
			return "", false, err
		}
		return target, isFilesystemPath(target), nil
	}
	return "", false, nil
}

// unquotePath removes go.mod quotes around one path token when present.
func unquotePath(path string) (string, error) {
	if !strings.HasPrefix(path, `"`) {
		return path, nil
	}
	unquoted, err := strconv.Unquote(path)
	if err != nil {
		return "", fmt.Errorf("parsing quoted replace target %s: %w", path, err)
	}
	return unquoted, nil
}

// isFilesystemPath reports whether a replace target names a local path.
func isFilesystemPath(path string) bool {
	return strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") || filepath.IsAbs(path)
}

// resolveReplaceRoot resolves target relative to module root.
func resolveReplaceRoot(root string, target string) string {
	assert.True(isFilesystemPath(target), "replace target %s must be a filesystem path", target)
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(root, target))
}

// additionalRoots returns roots other than the main module root.
func additionalRoots(roots []string, mainRoot string) []string {
	mainRoot, err := filepath.Abs(mainRoot)
	if err != nil {
		return nil
	}
	mainRoot = filepath.Clean(mainRoot)
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root != mainRoot {
			out = append(out, root)
		}
	}
	return out
}

// listAdditionalRoot lists an unselected local dependency module.
func listAdditionalRoot(tool gotool.Tool, root string) ([]gotool.Package, error) {
	packages, err := tool.ListPackages(root, []string{"./..."})
	if err != nil {
		return nil, fmt.Errorf("listing local module root %s: %w", root, err)
	}
	return markModuleDir(packages, root), nil
}

// markModuleDir annotates packages with the local module root that produced them.
func markModuleDir(packages []gotool.Package, root string) []gotool.Package {
	root, err := filepath.Abs(root)
	if err == nil {
		root = filepath.Clean(root)
	}
	out := append([]gotool.Package(nil), packages...)
	for index := range out {
		out[index].ModuleDir = root
	}
	return out
}

// wholeModulePattern reports whether patterns select the full module closure directly.
func wholeModulePattern(patterns []string) bool {
	return len(patterns) == 1 && patterns[0] == "./..."
}

// markSelected validates the deduplicated closure and marks pattern matches.
func markSelected(
	module gotool.Module,
	all []gotool.Package,
	selected []gotool.Package,
) ([]gotool.Package, error) {
	closure := dedupeByImportPath(all, selected)
	if err := rejectLoadErrors(closure); err != nil {
		return nil, err
	}
	if err := rejectSyntaxErrors(closure); err != nil {
		return nil, err
	}
	if err := rejectOutsideModule(module, all, selected); err != nil {
		return nil, err
	}
	selectedSet := make(map[string]bool, len(selected))
	for _, p := range selected {
		selectedSet[p.ImportPath] = true
	}
	markedCount := 0
	for index, p := range closure {
		if p.ImportPath == "" || p.Dir == "" {
			return nil, fmt.Errorf("go list emitted a package without identity: %+v", p)
		}
		closure[index].Selected = selectedSet[p.ImportPath]
		if closure[index].Selected {
			markedCount++
		}
	}
	// rejectOutsideModule guarantees every selected package is in the closure.
	assert.Equal(markedCount, len(selectedSet), "selected packages missing from closure")
	sort.Slice(closure, func(i, j int) bool {
		return closure[i].ImportPath < closure[j].ImportPath
	})
	return closure, nil
}

// dedupeByImportPath keeps the first occurrence of every import path across lists.
//
// Merging the selection listing with closure listings repeats shared packages,
// so validation dedupes once here instead of in every downstream check.
func dedupeByImportPath(lists ...[]gotool.Package) []gotool.Package {
	total := 0
	for _, packages := range lists {
		total += len(packages)
	}
	seen := make(map[string]bool, total)
	unique := make([]gotool.Package, 0, total)
	for _, packages := range lists {
		for _, p := range packages {
			if seen[p.ImportPath] {
				continue
			}
			seen[p.ImportPath] = true
			unique = append(unique, p)
		}
	}
	return unique
}

// rejectLoadErrors returns the first `go list` load error carried in package JSON.
//
// `go list` can exit zero while reporting syntax and load failures through the
// package Error field, so discovery reads that field instead of trusting only
// the process exit status.
func rejectLoadErrors(packages []gotool.Package) error {
	broken := make([]gotool.Package, 0)
	for _, p := range packages {
		if loadErrorMessage(p) != "" {
			broken = append(broken, p)
		}
	}
	if len(broken) == 0 {
		return nil
	}
	first := broken[0]
	if len(broken) == 1 {
		return fmt.Errorf("package %s is broken: %s", first.ImportPath, loadErrorMessage(first))
	}
	return fmt.Errorf("package %s is broken: %s (and %d more packages with errors)",
		first.ImportPath, loadErrorMessage(first), len(broken)-1)
}

// loadErrorMessage extracts the failure reason from `go list` package metadata.
func loadErrorMessage(p gotool.Package) string {
	if p.Error != nil && p.Error.Err != "" {
		return p.Error.Err
	}
	if p.Incomplete {
		return "go list reported the package as incomplete"
	}
	return ""
}

// rejectSyntaxErrors parses local package sources to catch broken files early.
func rejectSyntaxErrors(packages []gotool.Package) error {
	for _, p := range packages {
		if err := parsePackageSources(p); err != nil {
			return err
		}
	}
	return nil
}

// parsePackageSources parses Go and Cgo source files listed for one package.
func parsePackageSources(p gotool.Package) error {
	exists, err := packageDirExists(p.Dir)
	if err != nil {
		return fmt.Errorf("package %s is broken: checking %s: %w", p.ImportPath, p.Dir, err)
	}
	if !exists {
		return nil
	}
	for _, file := range packageSourceFiles(p) {
		path := filepath.Join(p.Dir, file)
		if _, err := parser.ParseFile(token.NewFileSet(), path, nil, 0); err != nil {
			return fmt.Errorf("package %s is broken: %s", p.ImportPath, err)
		}
	}
	return nil
}

// packageSourceFiles returns the source files that should parse as Go syntax.
func packageSourceFiles(p gotool.Package) []string {
	files := append([]string{}, p.GoFiles...)
	files = append(files, p.CgoFiles...)
	files = append(files, p.TestGoFiles...)
	files = append(files, p.XTestGoFiles...)
	return files
}

// packageDirExists reports whether dir exists and is a directory.
func packageDirExists(dir string) (bool, error) {
	if dir == "" {
		return false, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}

// rejectOutsideModule prevents silently dropping selected packages outside this module.
func rejectOutsideModule(
	module gotool.Module,
	all []gotool.Package,
	selected []gotool.Package,
) error {
	allSet := make(map[string]bool, len(all))
	for _, p := range all {
		allSet[p.ImportPath] = true
	}
	for _, p := range selected {
		if !packageInModule(p.ImportPath, module.Path) || !allSet[p.ImportPath] {
			return fmt.Errorf("package %s is outside module %s; rask tests the current module only",
				p.ImportPath, module.Path)
		}
	}
	return nil
}

// packageInModule reports whether importPath belongs to modulePath.
func packageInModule(importPath string, modulePath string) bool {
	return importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")
}
