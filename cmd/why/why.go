// Package why implements `rask why`, the cache-key input tracer.
//
// The command answers "why did this suite rerun instead of hitting the
// cache?" by listing every input that feeds a suite's cache key: module
// metadata behind the salt, the suite's test files and fixtures, the
// package's own build inputs, and the transitive in-closure dependency graph
// with per-package build digests. Running it before and after an unexpected
// rerun shows exactly which digest moved.
package why

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/chronark/rask/internal/cache"
	"github.com/chronark/rask/internal/cli"
	appconfig "github.com/chronark/rask/internal/config"
	"github.com/chronark/rask/internal/discover"
	"github.com/chronark/rask/internal/fingerprint"
	"github.com/chronark/rask/internal/frontend"
	"github.com/chronark/rask/internal/gotool"
	"github.com/chronark/rask/internal/setup"
)

var (
	// openCache is swappable because why owns command flow, not cache construction tests.
	openCache = func(cfg cache.Config) (suiteCache, error) { return cache.Open(cfg) }
)

// suiteCache is the narrow cache behavior needed to report hit-or-stale.
type suiteCache interface {
	Get(ctx context.Context, digest string) (cache.Entry, bool)
}

// Command returns the why command definition.
func Command() *cli.Command {
	return &cli.Command{
		Name:        "why",
		Usage:       "Trace every input behind a package's test cache key",
		Description: "Why takes package patterns and prints each selected suite's cache key, cache status, hashed files, and transitive dependency graph.",
		Examples: []string{
			"rask why ./internal/cache",
			"rask why ./...",
		},
		AcceptsArgs: true,
		Action:      Action,
	}
}

// Action traces cache-key inputs for the requested package patterns.
func Action(ctx context.Context, command *cli.Command) error {
	environment := setup.FromContext(ctx)
	code, err := run(ctx, environment, command)
	environment.ExitCode = code
	return err
}

// run keeps the command linear: discover, explain, check cache, render.
func run(ctx context.Context, environment *setup.Environment, command *cli.Command) (int, error) {
	patterns := command.Args()
	if len(patterns) == 0 {
		return 1, fmt.Errorf("why needs a package pattern, for example `rask why ./internal/cache`")
	}
	cfg, err := appconfig.Load(environment.Stderr)
	if err != nil {
		return 1, err
	}
	userDir, err := os.Getwd()
	if err != nil {
		return 1, fmt.Errorf("resolving working directory: %w", err)
	}
	module, err := environment.Tool.CurrentModule(ctx)
	if err != nil {
		return 1, err
	}
	packages, err := discover.Packages(ctx, environment.Tool, module, userDir, patterns)
	if err != nil {
		return 1, err
	}
	explanation, err := fingerprint.Explain(ctx, environment.Tool, module, packages)
	if err != nil {
		return 1, err
	}
	resultCache, err := openCache(cache.Config{
		ModulePath: module.Path,
		DiskDir:    cfg.CacheDir,
		Remote:     cfg.Remote,
	})
	if err != nil {
		return 1, err
	}
	environment.Frontend.RenderWhy(whyReport(ctx, module, packages, explanation, resultCache))
	return 0, nil
}

// whyReport adapts one explanation into the frontend's rendering shape.
func whyReport(
	ctx context.Context,
	module gotool.Module,
	packages []gotool.Package,
	explanation fingerprint.Explanation,
	resultCache suiteCache,
) frontend.WhyReport {
	suitesByPath := suiteTraceMap(explanation.Suites)
	report := frontend.WhyReport{ModulePath: module.Path}
	for _, p := range selectedPackages(packages) {
		trace, hasTests := suitesByPath[p.ImportPath]
		if !hasTests {
			report.Suites = append(report.Suites, frontend.WhySuite{ImportPath: p.ImportPath})
			continue
		}
		report.Suites = append(report.Suites, whySuite(ctx, trace, explanation, resultCache))
	}
	return report
}

// whySuite folds one suite trace, its cache status, and its graph into rows.
func whySuite(
	ctx context.Context,
	trace fingerprint.SuiteTrace,
	explanation fingerprint.Explanation,
	resultCache suiteCache,
) frontend.WhySuite {
	entry, cached := resultCache.Get(ctx, trace.Digest)
	build := explanation.Builds[trace.ImportPath]
	flattener := newWhyFlattener(explanation.Builds)
	flattener.seen[trace.ImportPath] = true
	dependencies := flattener.flatten(build.Imports)
	testOnly := flattener.flatten(trace.TestOnlyImports)
	return frontend.WhySuite{
		ImportPath:      trace.ImportPath,
		HasTests:        true,
		CacheKey:        trace.Digest,
		Cached:          cached,
		CachedPassed:    entry.Passed,
		CachedIgnored:   entry.Ignored,
		CachedAt:        entry.CreatedAt,
		SaltFiles:       saltFiles(explanation.Salt),
		TestFiles:       append(append([]string{}, trace.TestFiles...), trace.TestdataFiles...),
		PackageDigest:   build.Digest,
		PackageFiles:    build.Files,
		Dependencies:    dependencies,
		TestOnlyImports: testOnly,
	}
}

// saltFiles lists the module metadata files hashed into every cache key.
func saltFiles(salt fingerprint.SaltTrace) []string {
	files := make([]string, 0, 2*len(salt.Roots)+1)
	if salt.GoWork != "" {
		files = append(files, salt.GoWork)
	}
	for _, root := range salt.Roots {
		files = append(files, filepath.Join(root.Dir, "go.mod"))
		if root.GoSum {
			files = append(files, filepath.Join(root.Dir, "go.sum"))
		}
	}
	return files
}

// whyFlattener walks build imports depth-first with one shared seen set.
//
// The seen set spans a whole suite block so a package expanded under
// dependencies renders as a repeat under test-only imports instead of
// duplicating its subtree.
type whyFlattener struct {
	// builds resolves import paths to their traced digests and edges.
	builds map[string]fingerprint.BuildTrace

	// seen marks packages already expanded in this suite block.
	seen map[string]bool
}

// newWhyFlattener starts one suite block's shared walk state.
func newWhyFlattener(builds map[string]fingerprint.BuildTrace) *whyFlattener {
	return &whyFlattener{builds: builds, seen: make(map[string]bool)}
}

// flatten renders one section's import roots into depth-tagged rows.
func (f *whyFlattener) flatten(roots []string) []frontend.WhyDependency {
	rows := make([]frontend.WhyDependency, 0, len(roots))
	for _, importPath := range roots {
		rows = f.walk(rows, importPath, 0)
	}
	return rows
}

// walk appends one package row and recurses into unseen build imports.
//
// Recursion terminates because Explain already proved the closure acyclic and
// the seen set stops every revisit at one row.
func (f *whyFlattener) walk(
	rows []frontend.WhyDependency,
	importPath string,
	depth int,
) []frontend.WhyDependency {
	build := f.builds[importPath]
	if f.seen[importPath] {
		return append(rows, frontend.WhyDependency{
			ImportPath: importPath, Digest: build.Digest, Depth: depth, Repeated: true,
		})
	}
	f.seen[importPath] = true
	rows = append(rows, frontend.WhyDependency{
		ImportPath: importPath, Digest: build.Digest, Depth: depth,
	})
	for _, dep := range build.Imports {
		rows = f.walk(rows, dep, depth+1)
	}
	return rows
}

// suiteTraceMap indexes suite traces for selected-package folding.
func suiteTraceMap(suites []fingerprint.SuiteTrace) map[string]fingerprint.SuiteTrace {
	byPath := make(map[string]fingerprint.SuiteTrace, len(suites))
	for _, trace := range suites {
		byPath[trace.ImportPath] = trace
	}
	return byPath
}

// selectedPackages returns the user-selected packages in path order.
func selectedPackages(packages []gotool.Package) []gotool.Package {
	selected := make([]gotool.Package, 0, len(packages))
	for _, p := range packages {
		if p.Selected {
			selected = append(selected, p)
		}
	}
	// Discovery shuffles its return order on purpose; reports sort it back out.
	sort.Slice(selected, func(i int, j int) bool {
		return selected[i].ImportPath < selected[j].ImportPath
	})
	return selected
}
