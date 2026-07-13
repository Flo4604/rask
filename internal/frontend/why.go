package frontend

import (
	"fmt"
	"strings"
	"time"

	"github.com/chronark/rask/internal/assert"
)

const (
	// whySectionIndent leads section labels and the verdict block.
	whySectionIndent = "  "

	// whyEntryIndent leads file rows and dependency trees inside a section.
	whyEntryIndent = "    "

	// whyGuideWidth is the rune width of one tree-guide segment.
	whyGuideWidth = 3

	// whyShortDigestLength keeps per-package digests readable while still
	// distinguishing runs; the full cache key renders unabbreviated.
	whyShortDigestLength = 12

	// whyColumnGap is the minimum space between the left column and digests.
	whyColumnGap = 2

	// whyColumnMin clears every section label so right-hand cells share one
	// column: len("  test-only imports") + whyColumnGap.
	whyColumnMin = 21
)

// WhyReport is the rendering data for one `rask why` invocation.
type WhyReport struct {
	// ModulePath shortens in-module dependency rows, matching test output.
	ModulePath string

	// Suites are the traced packages in display order.
	Suites []WhySuite
}

// WhySuite is the rendering data for one traced package.
type WhySuite struct {
	// ImportPath is the traced package import path.
	ImportPath string

	// HasTests reports whether the package has a suite to cache at all.
	HasTests bool

	// CacheKey is the full suite digest used as the cache key.
	CacheKey string

	// Cached reports whether the cache currently holds CacheKey.
	Cached bool

	// CachedPassed is the stored passing test count for a cached suite.
	CachedPassed int

	// CachedIgnored is the stored skipped test count for a cached suite.
	CachedIgnored int

	// CachedAt is the stored entry creation time; zero hides entry detail.
	CachedAt time.Time

	// SaltFiles are module metadata files hashed into every cache key.
	SaltFiles []string

	// TestFiles are the suite's hashed test inputs, including testdata.
	TestFiles []string

	// PackageDigest is the package's own build digest.
	PackageDigest string

	// PackageFiles are the package's hashed non-test build inputs.
	PackageFiles []string

	// Dependencies are flattened transitive build imports in tree order.
	Dependencies []WhyDependency

	// TestOnlyImports are flattened test-only imports in tree order.
	TestOnlyImports []WhyDependency
}

// WhyDependency is one flattened dependency-tree row.
type WhyDependency struct {
	// ImportPath is the dependency import path.
	ImportPath string

	// Digest is the dependency build digest.
	Digest string

	// Depth is the tree depth below the section header, starting at zero.
	Depth int

	// Repeated marks a package already expanded earlier in this suite block.
	Repeated bool
}

// RenderWhy renders one why report; suites are separated by blank lines.
func (f *Frontend) RenderWhy(report WhyReport) {
	renderer := &whyRenderer{frontend: f, modulePath: report.ModulePath}
	for index, suite := range report.Suites {
		if index > 0 {
			fmt.Fprintln(f.out)
		}
		renderer.renderSuite(suite)
	}
	renderer.renderSummary(report.Suites)
}

// whyRenderer lays one report against a per-suite right-hand digest column.
//
// Digests and annotations share one column per suite block so two runs can be
// compared by scanning a single vertical edge for the digest that moved.
type whyRenderer struct {
	// frontend supplies the writer, color policy, and terminal width.
	frontend *Frontend

	// modulePath shortens in-module import paths in dependency rows.
	modulePath string

	// column is the current suite's right-hand column start.
	column int
}

// renderSuite renders one traced package block.
func (r *whyRenderer) renderSuite(suite WhySuite) {
	out := r.frontend.out
	fmt.Fprintf(out, "%s\n\n", r.frontend.paint(colorBold, "Suite "+suite.ImportPath))
	if !suite.HasTests {
		fmt.Fprintf(out, "%sno test files — nothing to run or cache\n", whySectionIndent)
		return
	}
	r.column = r.rightColumn(suite)
	r.renderVerdict(suite)
	// The Go environment always salts the key even though it is not a file.
	saltInputs := append([]string{"go env"}, suite.SaltFiles...)
	r.renderFileSection("module inputs", "hashed into every suite's cache key", saltInputs)
	r.renderFileSection("test files", "hashed into this suite only", suite.TestFiles)
	r.renderPackageFiles(suite)
	r.renderTreeSection("dependencies", "build digests of transitive imports", suite.Dependencies)
	r.renderTreeSection("test-only imports", "imported by test files only", suite.TestOnlyImports)
}

// renderVerdict renders the answer users came for before any input detail.
func (r *whyRenderer) renderVerdict(suite WhySuite) {
	out := r.frontend.out
	fmt.Fprintf(out, "%sstatus     %s\n", whySectionIndent, r.verdict(suite))
	fmt.Fprintf(out, "%scache key  %s\n", whySectionIndent, suite.CacheKey)
}

// verdict renders the cached-or-stale line with stored-entry detail.
func (r *whyRenderer) verdict(suite WhySuite) string {
	f := r.frontend
	if !suite.Cached {
		return f.paint(colorYellowBold, "stale") +
			f.paint(colorDim, " — no cached result for this key; next run executes go test")
	}
	detail := " — next run is a cache hit"
	if !suite.CachedAt.IsZero() {
		detail = fmt.Sprintf(" — %s, stored %s ago",
			whyCachedCounts(suite), whyAge(f.now().Sub(suite.CachedAt)))
	}
	return f.paint(colorGreenBold, "cached") + f.paint(colorDim, detail)
}

// whyCachedCounts summarizes the stored verdict a cache hit would replay.
func whyCachedCounts(suite WhySuite) string {
	counts := fmt.Sprintf("%d passed", suite.CachedPassed)
	if suite.CachedIgnored > 0 {
		counts += fmt.Sprintf("; %d ignored", suite.CachedIgnored)
	}
	return counts
}

// whyAge renders entry age at the precision a rerun decision needs.
func whyAge(elapsed time.Duration) string {
	switch {
	case elapsed < time.Minute:
		return "moments"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%dd", int(elapsed.Hours()/24))
	}
}

// renderFileSection renders one annotated label above its file rows.
func (r *whyRenderer) renderFileSection(label string, annotation string, entries []string) {
	if len(entries) == 0 {
		return
	}
	r.renderSectionLabel(label, annotation)
	for _, entry := range entries {
		fmt.Fprintf(r.frontend.out, "%s%s\n", whyEntryIndent, entry)
	}
}

// renderPackageFiles carries the suite's own build digest on the label row.
func (r *whyRenderer) renderPackageFiles(suite WhySuite) {
	if len(suite.PackageFiles) == 0 {
		return
	}
	r.renderSectionLabel("package files", "build digest "+shortWhyDigest(suite.PackageDigest))
	for _, entry := range suite.PackageFiles {
		fmt.Fprintf(r.frontend.out, "%s%s\n", whyEntryIndent, entry)
	}
}

// renderTreeSection renders flattened dependency rows as a guided tree.
func (r *whyRenderer) renderTreeSection(label string, annotation string, rows []WhyDependency) {
	if len(rows) == 0 {
		return
	}
	r.renderSectionLabel(label, annotation)
	guides := whyTreeGuides(rows)
	for index, row := range rows {
		r.renderTreeRow(guides[index], row)
	}
}

// renderSectionLabel starts a section with one blank line and a dim annotation.
func (r *whyRenderer) renderSectionLabel(label string, annotation string) {
	f := r.frontend
	left := whySectionIndent + label
	fmt.Fprintf(f.out, "\n%s%s\n", left, r.alignRight(len([]rune(left)), f.paint(colorDim, annotation)))
}

// renderTreeRow renders one dependency with its digest on the shared column.
func (r *whyRenderer) renderTreeRow(guide string, row WhyDependency) {
	f := r.frontend
	path := ShortenImportPath(row.ImportPath, r.modulePath)
	width := len([]rune(whyEntryIndent + guide + path))
	if row.Repeated {
		fmt.Fprintf(f.out, "%s%s%s\n", whyEntryIndent,
			f.paint(colorDim, guide+path), r.alignRight(width, f.paint(colorDim, "shown above")))
		return
	}
	fmt.Fprintf(f.out, "%s%s%s%s\n", whyEntryIndent, f.paint(colorDim, guide), path,
		r.alignRight(width, f.paint(colorDim, shortWhyDigest(row.Digest))))
}

// renderSummary closes multi-suite reports with one aggregate verdict line.
func (r *whyRenderer) renderSummary(suites []WhySuite) {
	if len(suites) < 2 {
		return
	}
	f := r.frontend
	cached, stale, testless := 0, 0, 0
	for _, suite := range suites {
		switch {
		case !suite.HasTests:
			testless++
		case suite.Cached:
			cached++
		default:
			stale++
		}
	}
	parts := make([]string, 0, 3)
	if cached > 0 {
		parts = append(parts, f.paint(colorGreenBold, fmt.Sprintf("%d cached", cached)))
	}
	if stale > 0 {
		parts = append(parts, f.paint(colorYellowBold, fmt.Sprintf("%d stale", stale)))
	}
	if testless > 0 {
		parts = append(parts, fmt.Sprintf("%d without tests", testless))
	}
	fmt.Fprintf(f.out, "\n%s %d %s: %s\n", f.paint(colorBold, "Traced"),
		len(suites), plural(len(suites), "suite"), strings.Join(parts, ", "))
}

// alignRight pads to the suite column so right-hand cells share one edge.
func (r *whyRenderer) alignRight(visibleWidth int, right string) string {
	padding := r.column - visibleWidth
	if padding < whyColumnGap {
		padding = whyColumnGap
	}
	return strings.Repeat(" ", padding) + right
}

// rightColumn fits every aligned row of one suite inside the terminal width.
func (r *whyRenderer) rightColumn(suite WhySuite) int {
	column := whyColumnMin
	for _, rows := range [][]WhyDependency{suite.Dependencies, suite.TestOnlyImports} {
		for _, row := range rows {
			path := ShortenImportPath(row.ImportPath, r.modulePath)
			width := len([]rune(whyEntryIndent)) +
				whyGuideWidth*(row.Depth+1) + len([]rune(path))
			column = max(column, width+whyColumnGap)
		}
	}
	return min(column, max(whyColumnMin, r.frontend.width-whyShortDigestLength-1))
}

// whyTreeGuides converts depth-tagged rows into branch guide prefixes.
//
// A row is a last sibling when no later row shares its depth before the walk
// returns to a shallower depth; open branches draw continuation bars through
// the rows of their expanded subtree.
func whyTreeGuides(rows []WhyDependency) []string {
	guides := make([]string, len(rows))
	openBranch := make(map[int]bool, 4)
	for index, row := range rows {
		assert.GreaterOrEqual(row.Depth, 0)
		last := whyLastSibling(rows, index)
		guide := new(strings.Builder)
		for depth := 0; depth < row.Depth; depth++ {
			if openBranch[depth] {
				guide.WriteString("│  ")
			} else {
				guide.WriteString("   ")
			}
		}
		if last {
			guide.WriteString("└─ ")
		} else {
			guide.WriteString("├─ ")
		}
		openBranch[row.Depth] = !last
		guides[index] = guide.String()
	}
	return guides
}

// whyLastSibling reports whether no sibling follows rows[index] in its subtree.
func whyLastSibling(rows []WhyDependency, index int) bool {
	for _, later := range rows[index+1:] {
		if later.Depth < rows[index].Depth {
			return true
		}
		if later.Depth == rows[index].Depth {
			return false
		}
	}
	return true
}

// shortWhyDigest abbreviates build digests for aligned rows.
func shortWhyDigest(digest string) string {
	assert.GreaterOrEqual(len(digest), whyShortDigestLength)
	return digest[:whyShortDigestLength]
}
