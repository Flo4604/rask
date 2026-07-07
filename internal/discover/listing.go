package discover

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/chronark/rask/internal/gotool"
)

// discover keeps the closure build separate from the public shuffle contract.
func (d *discoverer) discover(ctx context.Context) ([]gotool.Package, error) {
	roots, err := LocalModuleRoots(ctx, d.tool, d.module)
	if err != nil {
		return nil, err
	}
	selected, err := d.listSelected(ctx)
	if err != nil {
		return nil, err
	}
	closure, err := d.listMainClosure(ctx, selected)
	if err != nil {
		return nil, err
	}
	closure, err = d.appendAdditionalRoots(ctx, closure, roots)
	if err != nil {
		return nil, err
	}
	return d.validateAndMark(closure, selected)
}

// listSelected fails fast so an empty user pattern cannot look like success.
func (d *discoverer) listSelected(ctx context.Context) ([]gotool.Package, error) {
	selected, err := d.tool.ListPackages(ctx, d.patternDir, d.patterns)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("package pattern %s matched no packages", strings.Join(d.patterns, " "))
	}
	return selected, nil
}

// listMainClosure avoids a redundant go-list when ./... is already complete.
func (d *discoverer) listMainClosure(
	ctx context.Context,
	selected []gotool.Package,
) ([]gotool.Package, error) {
	if wholeModulePattern(d.patterns) {
		return markModuleDir(selected, d.module.Dir), nil
	}
	packages, err := d.tool.ListPackages(ctx, d.module.Dir, []string{"./..."})
	if err != nil {
		return nil, err
	}
	return markModuleDir(packages, d.module.Dir), nil
}

// appendAdditionalRoots keeps local module dependencies in the hash universe.
func (d *discoverer) appendAdditionalRoots(
	ctx context.Context,
	closure []gotool.Package,
	roots []string,
) ([]gotool.Package, error) {
	for _, root := range additionalRoots(roots, d.module.Dir) {
		packages, err := d.listAdditionalRoot(ctx, root)
		if err != nil {
			return nil, err
		}
		closure = append(closure, packages...)
	}
	return closure, nil
}

// listAdditionalRoot names the local dependency root in go-list failures.
func (d *discoverer) listAdditionalRoot(
	ctx context.Context,
	root string,
) ([]gotool.Package, error) {
	packages, err := d.tool.ListPackages(ctx, root, []string{"./..."})
	if err != nil {
		return nil, fmt.Errorf("listing local module root %s: %w", root, err)
	}
	return markModuleDir(packages, root), nil
}

// markModuleDir records ownership so later stages can salt by module root.
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

// wholeModulePattern is intentionally narrow to avoid under-listing closures.
func wholeModulePattern(patterns []string) bool {
	return len(patterns) == 1 && patterns[0] == "./..."
}
