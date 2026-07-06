package discover

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/chronark/rask/internal/gotool"
)

// discover builds the validated closure and marks packages matched by patterns.
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
	selected, err := listSelectedPackages(tool, patternDir, patterns)
	if err != nil {
		return nil, err
	}
	closure, err := listMainClosure(tool, module, patterns, selected)
	if err != nil {
		return nil, err
	}
	closure, err = appendAdditionalRootPackages(tool, closure, roots, module.Dir)
	if err != nil {
		return nil, err
	}
	return validateAndMarkClosure(module, closure, selected)
}

// listSelectedPackages rejects zero-package pattern matches before hashing.
func listSelectedPackages(
	tool gotool.Tool,
	patternDir string,
	patterns []string,
) ([]gotool.Package, error) {
	selected, err := tool.ListPackages(patternDir, patterns)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("package pattern %s matched no packages", strings.Join(patterns, " "))
	}
	return selected, nil
}

// listMainClosure reuses the selection walk when the user asked for ./....
func listMainClosure(
	tool gotool.Tool,
	module gotool.Module,
	patterns []string,
	selected []gotool.Package,
) ([]gotool.Package, error) {
	if wholeModulePattern(patterns) {
		return markModuleDir(selected, module.Dir), nil
	}
	packages, err := tool.ListPackages(module.Dir, []string{"./..."})
	if err != nil {
		return nil, err
	}
	return markModuleDir(packages, module.Dir), nil
}

// appendAdditionalRootPackages includes local replace and workspace modules.
func appendAdditionalRootPackages(
	tool gotool.Tool,
	closure []gotool.Package,
	roots []string,
	mainRoot string,
) ([]gotool.Package, error) {
	for _, root := range additionalRoots(roots, mainRoot) {
		packages, err := listAdditionalRoot(tool, root)
		if err != nil {
			return nil, err
		}
		closure = append(closure, packages...)
	}
	return closure, nil
}

// listAdditionalRoot wraps local-root go-list failures with the root path.
func listAdditionalRoot(tool gotool.Tool, root string) ([]gotool.Package, error) {
	packages, err := tool.ListPackages(root, []string{"./..."})
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
