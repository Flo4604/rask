package fingerprint

import (
	"context"
	"sort"

	"github.com/chronark/rask/internal/gotool"
)

// Explanation reports every content input behind the selected suite digests.
//
// It exists so `rask why` can show users exactly which files and packages feed
// a cache key. All digests come from the same hashing code path as [Suites],
// so a traced digest always equals the cache key a test run would compute.
type Explanation struct {
	// Salt describes the global inputs shared by every suite digest.
	Salt SaltTrace

	// Builds holds one build trace per package in the discovered closure.
	Builds map[string]BuildTrace

	// Suites holds selected packages with tests, sorted by import path.
	Suites []SuiteTrace
}

// SaltTrace lists the global metadata inputs folded into every digest.
type SaltTrace struct {
	// Digest is the shared salt over go env, go.work, and module files.
	Digest string

	// GoWork is the active workspace file path, or empty without one.
	GoWork string

	// Roots are the hashed local module roots in salt order.
	Roots []SaltRoot
}

// SaltRoot records one local module root's hashed metadata files.
type SaltRoot struct {
	// Dir is the canonical module root directory.
	Dir string

	// GoSum reports whether the root's go.sum existed and was hashed.
	GoSum bool
}

// BuildTrace records one closure package's build-digest inputs.
type BuildTrace struct {
	// ImportPath is the traced package import path.
	ImportPath string

	// Digest is the package build digest.
	Digest string

	// Files are the hashed non-test build inputs relative to the package dir.
	Files []string

	// Imports are the package's direct build imports inside the closure.
	Imports []string
}

// SuiteTrace records one selected suite's terminal-digest inputs.
type SuiteTrace struct {
	// ImportPath is the selected package import path.
	ImportPath string

	// Digest is the suite digest used as the cache key.
	Digest string

	// TestFiles are the hashed test inputs relative to the package dir.
	TestFiles []string

	// TestdataFiles are hashed fixture paths relative to the package dir.
	TestdataFiles []string

	// TestOnlyImports are in-closure test imports beyond the build imports.
	TestOnlyImports []string
}

// Explain traces the fingerprint inputs for every selected suite in packages.
//
// Callers pass the same discovered universe they would pass to [Suites]; the
// returned digests are byte-identical to the cache keys of a test run over
// that universe.
func Explain(
	ctx context.Context,
	tool gotool.Tool,
	module gotool.Module,
	packages []gotool.Package,
) (Explanation, error) {
	salt, err := hashSaltTraced(ctx, tool, module)
	if err != nil {
		return Explanation{}, err
	}
	buildDigests, buildOrder, err := buildDigestMap(packages, salt.Digest, randomRand())
	if err != nil {
		return Explanation{}, err
	}
	byPath := packageMap(packages)
	builds := make(map[string]BuildTrace, len(buildOrder))
	for _, importPath := range buildOrder {
		builds[importPath] = buildTrace(byPath[importPath], buildDigests, byPath)
	}
	suites, err := suiteTraces(packages, buildDigests, salt.Digest)
	if err != nil {
		return Explanation{}, err
	}
	return Explanation{Salt: salt, Builds: builds, Suites: suites}, nil
}

// buildTrace pairs one package's build digest with the inputs that produced it.
func buildTrace(
	p gotool.Package,
	buildDigests map[string]string,
	byPath map[string]gotool.Package,
) BuildTrace {
	return BuildTrace{
		ImportPath: p.ImportPath,
		Digest:     buildDigests[p.ImportPath],
		Files:      sortedFileUnion(p, p.GoFiles, p.CgoFiles, p.SFiles, p.EmbedFiles),
		Imports:    buildImports(p, byPath),
	}
}

// suiteTraces re-derives terminal digests for selected suites in path order.
func suiteTraces(
	packages []gotool.Package,
	buildDigests map[string]string,
	salt string,
) ([]SuiteTrace, error) {
	selected := selectedTestPackages(packages)
	suites := make([]SuiteTrace, 0, len(selected))
	for _, p := range selected {
		trace, err := suiteTrace(p, buildDigests, salt)
		if err != nil {
			return nil, err
		}
		suites = append(suites, trace)
	}
	return suites, nil
}

// suiteTrace records one suite's cache key beside the inputs that formed it.
func suiteTrace(
	p gotool.Package,
	buildDigests map[string]string,
	salt string,
) (SuiteTrace, error) {
	digest, err := hashSuite(p, buildDigests, salt)
	if err != nil {
		return SuiteTrace{}, err
	}
	testdata, err := suiteTestdataFiles(p.Dir)
	if err != nil {
		return SuiteTrace{}, err
	}
	return SuiteTrace{
		ImportPath: p.ImportPath,
		Digest:     digest,
		TestFiles: sortedFileUnion(p, p.TestGoFiles, p.XTestGoFiles,
			p.TestEmbedFiles, p.XTestEmbedFiles),
		TestdataFiles:   testdata,
		TestOnlyImports: testOnlyImports(p, buildDigests),
	}, nil
}

// selectedTestPackages returns selected packages with tests in path order.
func selectedTestPackages(packages []gotool.Package) []gotool.Package {
	selected := make([]gotool.Package, 0, len(packages))
	for _, p := range packages {
		if p.Selected && p.HasTests() {
			selected = append(selected, p)
		}
	}
	sort.Slice(selected, func(i int, j int) bool {
		return selected[i].ImportPath < selected[j].ImportPath
	})
	return selected
}
