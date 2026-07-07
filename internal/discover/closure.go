package discover

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/gotool"
)

// validateAndMark enforces the package invariants fingerprinting trusts.
func (d *discoverer) validateAndMark(
	closureListing []gotool.Package,
	selected []gotool.Package,
) ([]gotool.Package, error) {
	if err := requirePackageIdentities(closureListing); err != nil {
		return nil, err
	}
	if err := requirePackageIdentities(selected); err != nil {
		return nil, err
	}
	if err := rejectOutsideModule(d.module, closureListing, selected); err != nil {
		return nil, err
	}
	closure := dedupeByImportPath(closureListing, selected)
	if err := rejectLoadErrors(closure); err != nil {
		return nil, err
	}
	if err := rejectSyntaxErrors(closure); err != nil {
		return nil, err
	}
	selectedPaths := selectedImportPaths(selected)
	markedCount := markSelectedPackages(closure, selectedPaths)
	assert.Equal(markedCount, len(selectedPaths), "selected packages missing from closure")
	sort.Slice(closure, func(i int, j int) bool {
		return closure[i].ImportPath < closure[j].ImportPath
	})
	return closure, nil
}

// requirePackageIdentities rejects malformed go-list records before deduping.
func requirePackageIdentities(packages []gotool.Package) error {
	for _, p := range packages {
		if p.ImportPath == "" || p.Dir == "" {
			return fmt.Errorf("go list emitted a package without identity: %+v", p)
		}
	}
	return nil
}

// dedupeByImportPath keeps the first package for each import path.
func dedupeByImportPath(lists ...[]gotool.Package) []gotool.Package {
	total := 0
	for _, packages := range lists {
		total += len(packages)
	}
	seen := make(map[string]bool, total)
	unique := make([]gotool.Package, 0, total)
	for _, packages := range lists {
		unique = appendUnseenPackages(unique, seen, packages)
	}
	return unique
}

// appendUnseenPackages preserves closure-listing precedence over selection.
func appendUnseenPackages(
	unique []gotool.Package,
	seen map[string]bool,
	packages []gotool.Package,
) []gotool.Package {
	for _, p := range packages {
		if seen[p.ImportPath] {
			continue
		}
		seen[p.ImportPath] = true
		unique = append(unique, p)
	}
	return unique
}

// rejectLoadErrors catches go-list JSON failures even when go exits zero.
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

// loadErrorMessage normalizes the two broken-package signals from go list.
func loadErrorMessage(p gotool.Package) string {
	if p.Error != nil && p.Error.Err != "" {
		return p.Error.Err
	}
	if p.Incomplete {
		return "go list reported the package as incomplete"
	}
	return ""
}

// rejectSyntaxErrors parses local sources before hashes hide parse failures.
func rejectSyntaxErrors(packages []gotool.Package) error {
	for _, p := range packages {
		if err := parsePackageSources(p); err != nil {
			return err
		}
	}
	return nil
}

// parsePackageSources validates every Go source file go-list attached.
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

// packageSourceFiles returns files that must parse before fingerprinting.
func packageSourceFiles(p gotool.Package) []string {
	files := append([]string{}, p.GoFiles...)
	files = append(files, p.CgoFiles...)
	files = append(files, p.TestGoFiles...)
	files = append(files, p.XTestGoFiles...)
	return files
}

// packageDirExists distinguishes vanished directories from stat failures.
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

// rejectOutsideModule prevents silent drops of selected external packages.
func rejectOutsideModule(
	module gotool.Module,
	closureListing []gotool.Package,
	selected []gotool.Package,
) error {
	closurePaths := packagePathSet(closureListing)
	for _, p := range selected {
		if packageInModule(p.ImportPath, module.Path) && closurePaths[p.ImportPath] {
			continue
		}
		return fmt.Errorf("package %s is outside module %s; rask tests the current module only",
			p.ImportPath, module.Path)
	}
	return nil
}

// packagePathSet records import paths that are present in the closure listing.
func packagePathSet(packages []gotool.Package) map[string]bool {
	paths := make(map[string]bool, len(packages))
	for _, p := range packages {
		paths[p.ImportPath] = true
	}
	return paths
}

// packageInModule reports whether an import path belongs to the main module.
func packageInModule(importPath string, modulePath string) bool {
	return importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")
}

// selectedImportPaths dedupes selected packages before the mark invariant.
func selectedImportPaths(selected []gotool.Package) map[string]bool {
	paths := make(map[string]bool, len(selected))
	for _, p := range selected {
		paths[p.ImportPath] = true
	}
	return paths
}

// markSelectedPackages mutates the validated closure with the selected set.
func markSelectedPackages(closure []gotool.Package, selected map[string]bool) int {
	marked := 0
	for index, p := range closure {
		closure[index].Selected = selected[p.ImportPath]
		if closure[index].Selected {
			marked++
		}
	}
	return marked
}
