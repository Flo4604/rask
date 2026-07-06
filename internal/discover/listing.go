package discover

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/chronark/rask/internal/gotool"
)

// discover builds the validated closure before Run decides emission order.
func (d *Discoverer) discover() ([]gotool.Package, error) {
	roots, err := LocalModuleRoots(d.tool, d.module)
	if err != nil {
		return nil, err
	}
	selected, err := d.listSelected()
	if err != nil {
		return nil, err
	}
	closure, err := d.listMainClosure(selected)
	if err != nil {
		return nil, err
	}
	closure, err = d.appendAdditionalRoots(closure, roots)
	if err != nil {
		return nil, err
	}
	return d.validateAndMark(closure, selected)
}

// listSelected rejects zero-package pattern matches before hashing.
func (d *Discoverer) listSelected() ([]gotool.Package, error) {
	selected, err := d.tool.ListPackages(d.patternDir, d.patterns)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("package pattern %s matched no packages", strings.Join(d.patterns, " "))
	}
	return selected, nil
}

// listMainClosure reuses the selection walk when it already covers the module.
func (d *Discoverer) listMainClosure(selected []gotool.Package) ([]gotool.Package, error) {
	if wholeModulePattern(d.patterns) {
		return markModuleDir(selected, d.module.Dir), nil
	}
	packages, err := d.tool.ListPackages(d.module.Dir, []string{"./..."})
	if err != nil {
		return nil, err
	}
	return markModuleDir(packages, d.module.Dir), nil
}

// appendAdditionalRoots includes local replace and workspace modules.
func (d *Discoverer) appendAdditionalRoots(
	closure []gotool.Package,
	roots []string,
) ([]gotool.Package, error) {
	for _, root := range additionalRoots(roots, d.module.Dir) {
		packages, err := d.listAdditionalRoot(root)
		if err != nil {
			return nil, err
		}
		closure = append(closure, packages...)
	}
	return closure, nil
}

// listAdditionalRoot wraps local-root go-list failures with the root path.
func (d *Discoverer) listAdditionalRoot(root string) ([]gotool.Package, error) {
	packages, err := d.tool.ListPackages(root, []string{"./..."})
	if err != nil {
		return nil, fmt.Errorf("listing local module root %s: %w", root, err)
	}
	return markModuleDir(packages, root), nil
}

// markModuleDir stamps packages with the canonical root that produced them.
func markModuleDir(packages []gotool.Package, root string) []gotool.Package {
	root, err := canonicalPath(root)
	if err != nil {
		root = filepath.Clean(root)
	}
	out := append([]gotool.Package(nil), packages...)
	for index := range out {
		out[index].ModuleDir = root
	}
	return out
}

// wholeModulePattern names the one pattern that already lists the full closure.
func wholeModulePattern(patterns []string) bool {
	return len(patterns) == 1 && patterns[0] == "./..."
}
