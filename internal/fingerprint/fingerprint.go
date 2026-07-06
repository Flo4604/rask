// Package fingerprint computes content digests for test suites.
//
// Fingerprinting uses two phases. First, package build digests are computed in
// topological order over build imports. Second, selected test suites become
// terminal digests over their test files, their package build digest, and
// discovered local test-only imports.
//
// Only build imports are topological edges. External test packages may import a
// package that imports the tested package back, so test imports are deliberately
// excluded from the graph and added only while hashing suite sinks.
//
// [Fingerprinter.Run] is a lifecycle wrapper over the deterministic hashing
// operation. It starts no internal goroutines; concurrent runs are safe when
// each run has its own channels and the supplied [gotool.Tool] and filesystem
// are stable.
package fingerprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/discover"
	"github.com/chronark/rask/internal/gotool"
)

const (
	// formatVersion separates incompatible cache and fingerprint formats.
	formatVersion = "v1"

	// filesPerPackageMax bounds file lists from malformed or unexpectedly large packages.
	filesPerPackageMax = 32768

	// hashHexLength is the length of a SHA-256 digest rendered as lowercase hex.
	hashHexLength = 64
)

// Suite identifies one selected package test suite and its content digest.
type Suite struct {
	// Package is the selected Go package to test.
	Package gotool.Package

	// Digest is the suite content digest used as the cache key.
	Digest string
}

// Fingerprinter consumes packages and streams cacheable suite digests.
//
// Fingerprinter is safe to run once. Run owns the suite channel: it closes it
// before returning, consumes packages until the input closes or ctx is
// cancelled, then computes build digests in dependency order and emits selected
// testable suites.
type Fingerprinter struct {
	tool   gotool.Tool
	module gotool.Module
	in     <-chan gotool.Package
	out    chan<- Suite
}

// New returns a fingerprinter connected to package and suite channels.
func New(
	tool gotool.Tool,
	module gotool.Module,
	in <-chan gotool.Package,
	out chan<- Suite,
) *Fingerprinter {
	return &Fingerprinter{tool: tool, module: module, in: in, out: out}
}

// fingerprint returns cacheable suite digests for selected packages with tests.
//
// The salt includes the cache format version, stable `go env` lines, go.work
// when active, and module metadata. Packages without tests still contribute
// build digests for dependents.
func fingerprint(
	tool gotool.Tool,
	module gotool.Module,
	packages []gotool.Package,
) ([]Suite, error) {
	salt, err := hashSalt(tool, module)
	if err != nil {
		return nil, err
	}
	buildDigests, err := buildDigestMap(packages, salt)
	if err != nil {
		return nil, err
	}
	suites := make([]Suite, 0, len(packages))
	for _, p := range packages {
		if !p.Selected || !p.HasTests() {
			continue
		}
		digest, err := hashSuite(p, buildDigests, salt)
		if err != nil {
			return nil, err
		}
		suites = append(suites, Suite{Package: p, Digest: digest})
	}
	sort.Slice(suites, func(i, j int) bool {
		return suites[i].Package.ImportPath < suites[j].Package.ImportPath
	})
	return suites, nil
}

// Run consumes packages, fingerprints suites, emits them, and closes the suite channel.
//
// Run is the suite channel's only sender and closes it with defer before
// returning. Every send selects on ctx.Done so cancellation can unblock
// downstream backpressure without panicking or leaking a goroutine.
func (f *Fingerprinter) Run(ctx context.Context) error {
	defer close(f.out)
	packages, err := f.collect(ctx)
	if err != nil {
		return err
	}
	suites, err := fingerprint(f.tool, f.module, packages)
	if err != nil {
		return err
	}
	for _, suite := range suites {
		select {
		case f.out <- suite:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// collect drains input until close so unresolved imports can be checked at stream end.
func (f *Fingerprinter) collect(ctx context.Context) ([]gotool.Package, error) {
	packages := make([]gotool.Package, 0, 256)
	for {
		select {
		case p, ok := <-f.in:
			if !ok {
				return packages, nil
			}
			packages = append(packages, p)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// buildDigestMap computes build digests in dependency order.
func buildDigestMap(
	packages []gotool.Package,
	salt string,
) (map[string]string, error) {
	byPath := packageMap(packages)
	pending, dependents := buildGraph(packages, byPath)
	ready := readyPackages(pending)
	buildDigests := make(map[string]string, len(packages))
	for index := 0; index < len(ready); index++ {
		importPath := ready[index]
		digest, err := hashBuild(byPath[importPath], buildDigests, salt)
		if err != nil {
			return nil, err
		}
		buildDigests[importPath] = digest
		for _, dependent := range dependents[importPath] {
			pending[dependent]--
			assert.GreaterOrEqual(pending[dependent], 0)
			if pending[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	if len(buildDigests) != len(packages) {
		return nil, fmt.Errorf("unresolved discovered imports prevented fingerprinting")
	}
	return buildDigests, nil
}

// buildGraph returns Kahn-sort state over discovered build imports.
func buildGraph(
	packages []gotool.Package,
	byPath map[string]gotool.Package,
) (map[string]int, map[string][]string) {
	pending := make(map[string]int, len(packages))
	dependents := make(map[string][]string, len(packages))
	for _, p := range packages {
		pending[p.ImportPath] = 0
		for _, dep := range buildImports(p, byPath) {
			pending[p.ImportPath]++
			dependents[dep] = append(dependents[dep], p.ImportPath)
		}
	}
	for importPath := range dependents {
		sort.Strings(dependents[importPath])
	}
	return pending, dependents
}

// readyPackages returns graph nodes with no pending build dependencies.
func readyPackages(pending map[string]int) []string {
	ready := make([]string, 0, len(pending))
	for importPath, count := range pending {
		if count == 0 {
			ready = append(ready, importPath)
		}
	}
	sort.Strings(ready)
	return ready
}

// packageMap indexes packages by import path and asserts discovery uniqueness.
func packageMap(packages []gotool.Package) map[string]gotool.Package {
	byPath := make(map[string]gotool.Package, len(packages))
	for _, p := range packages {
		if _, duplicate := byPath[p.ImportPath]; duplicate {
			assert.Unreachable("package %s arrived twice", p.ImportPath)
		}
		byPath[p.ImportPath] = p
	}
	return byPath
}

// hashSalt hashes inputs that should invalidate every suite in the run.
func hashSalt(tool gotool.Tool, module gotool.Module) (string, error) {
	environment, err := tool.Environment(module.Dir)
	if err != nil {
		return "", err
	}
	roots, err := discover.LocalModuleRootsFromEnvironment(tool, module, environment)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	writeHashString(digest, formatVersion)
	writeHashBytes(digest, gotool.SaltEnvironment(environment))
	if goWork := discover.GoWorkPath(environment); goWork != "" {
		writeHashString(digest, "go.work")
		if err := hashRequiredFile(digest, goWork); err != nil {
			return "", err
		}
	}
	for _, root := range roots {
		writeHashString(digest, root)
		if err := hashRequiredFile(digest, filepath.Join(root, "go.mod")); err != nil {
			return "", err
		}
		if err := hashOptionalFile(digest, filepath.Join(root, "go.sum")); err != nil {
			return "", err
		}
	}
	return finishDigest(digest), nil
}

// hashRequiredFile adds a required metadata file to digest.
func hashRequiredFile(digest hash.Hash, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	writeHashBytes(digest, body)
	return nil
}

// hashOptionalFile adds an optional module file to digest when it exists.
// Stat follows symlinks so a dangling link is skipped exactly like a missing file.
func hashOptionalFile(digest hash.Hash, path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return hashRequiredFile(digest, path)
}

// hashBuild hashes one package's non-test build inputs and build dependencies.
func hashBuild(
	p gotool.Package,
	buildDigests map[string]string,
	salt string,
) (string, error) {
	digest := saltedDigest(salt, "build\x00", p.ImportPath)
	files := sortedFileUnion(p, p.GoFiles, p.CgoFiles, p.SFiles, p.EmbedFiles)
	if err := hashNamedFiles(digest, p.Dir, files); err != nil {
		return "", err
	}
	for _, dep := range buildImports(p, buildDigests) {
		writeHashString(digest, buildDigests[dep])
	}
	return finishDigest(digest), nil
}

// hashSuite hashes one package's test inputs as a terminal suite digest.
func hashSuite(
	p gotool.Package,
	buildDigests map[string]string,
	salt string,
) (string, error) {
	digest := saltedDigest(salt, "suite\x00", p.ImportPath)
	files := sortedFileUnion(p, p.TestGoFiles, p.XTestGoFiles,
		p.TestEmbedFiles, p.XTestEmbedFiles)
	if err := hashNamedFiles(digest, p.Dir, files); err != nil {
		return "", err
	}
	if err := hashTestdata(digest, p.Dir); err != nil {
		return "", err
	}
	writeHashString(digest, buildDigests[p.ImportPath])
	for _, dep := range testOnlyImports(p, buildDigests) {
		depDigest, ok := buildDigests[dep]
		if !ok {
			return "", fmt.Errorf("package %s test-imports missing package %s", p.ImportPath, dep)
		}
		writeHashString(digest, depDigest)
	}
	return finishDigest(digest), nil
}

// saltedDigest starts a digest; the kind label keeps build and suite streams disjoint.
func saltedDigest(salt string, kind string, importPath string) hash.Hash {
	digest := sha256.New()
	writeHashString(digest, salt)
	writeHashString(digest, kind)
	writeHashString(digest, importPath)
	return digest
}

// hashNamedFiles hashes each name and its file content under dir into digest.
func hashNamedFiles(digest hash.Hash, dir string, names []string) error {
	for _, name := range names {
		writeHashString(digest, name)
		if err := hashFileInto(digest, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// sortedFileUnion combines file groups into a stable sorted list.
func sortedFileUnion(p gotool.Package, groups ...[]string) []string {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	files := make([]string, 0, total)
	for _, group := range groups {
		files = append(files, group...)
	}
	if len(files) > filesPerPackageMax {
		assert.Unreachable("%s has too many files", p.ImportPath)
	}
	sort.Strings(files)
	return files
}

// hashTestdata folds runtime fixtures into the suite digest when present.
func hashTestdata(digest hash.Hash, packageDir string) error {
	names, err := testdataFiles(filepath.Join(packageDir, "testdata"))
	if err != nil {
		return err
	}
	for index := range names {
		names[index] = "testdata/" + names[index]
	}
	return hashNamedFiles(digest, packageDir, names)
}

// testdataFiles returns sorted regular fixture paths under root, bounded per package.
// Non-regular entries are skipped so symlinked or special fixtures cannot block hashing.
func testdataFiles(root string) ([]string, error) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("checking testdata: %w", err)
		}
		return nil, nil
	}
	files := make([]string, 0)
	collect := func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		if len(files) >= filesPerPackageMax {
			return fmt.Errorf("testdata under %s exceeds %d files", root, filesPerPackageMax)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativizing testdata file: %w", err)
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	}
	if err := filepath.WalkDir(root, collect); err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// buildImports returns discovered imports that participate in package compilation.
func buildImports[T any](p gotool.Package, closure map[string]T) []string {
	return importsInClosure(p.ImportPath, closure, p.Imports)
}

// testOnlyImports returns discovered test imports that were not already build imports.
func testOnlyImports[T any](p gotool.Package, closure map[string]T) []string {
	buildSet := make(map[string]bool, len(p.Imports))
	for _, dep := range buildImports(p, closure) {
		buildSet[dep] = true
	}
	imports := importsInClosure(p.ImportPath, closure, p.TestImports, p.XTestImports)
	out := imports[:0]
	for _, dep := range imports {
		if !buildSet[dep] {
			out = append(out, dep)
		}
	}
	return out
}

// importsInClosure returns sorted unique imports present in the discovered closure.
func importsInClosure[T any](importer string, closure map[string]T, groups ...[]string) []string {
	seen := make(map[string]bool)
	for _, group := range groups {
		for _, importPath := range group {
			if importPath == importer {
				continue
			}
			if _, ok := closure[importPath]; ok {
				seen[importPath] = true
			}
		}
	}
	imports := make([]string, 0, len(seen))
	for importPath := range seen {
		imports = append(imports, importPath)
	}
	sort.Strings(imports)
	return imports
}

// finishDigest renders a digest and asserts it is a cache-safe SHA-256 hex key.
func finishDigest(digest hash.Hash) string {
	result := hex.EncodeToString(digest.Sum(nil))
	assert.Equal(len(result), hashHexLength)
	assert.False(strings.ContainsAny(result, "/\\"))
	return result
}

// hashFileInto streams regular files only so special files cannot block hashing.
func hashFileInto(digest hash.Hash, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if err != nil {
			return fmt.Errorf("hashing %s: %w", path, err)
		}
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("hashing %s: not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", path, err)
	}
	defer file.Close()
	if _, err := io.Copy(digest, file); err != nil {
		return fmt.Errorf("hashing %s: %w", path, err)
	}
	return nil
}

// writeHashString writes text into digest and asserts the hash accepted all bytes.
func writeHashString(digest hash.Hash, text string) {
	written, err := io.WriteString(digest, text)
	assert.NoError(err)
	assert.Equal(written, len(text))
}

// writeHashBytes writes body into digest and asserts the hash accepted all bytes.
func writeHashBytes(digest hash.Hash, body []byte) {
	written, err := digest.Write(body)
	assert.NoError(err)
	assert.Equal(written, len(body))
}
