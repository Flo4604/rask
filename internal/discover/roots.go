package discover

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/gotool"
)

// LocalModuleRoots returns sorted local roots that affect fingerprint salts.
func LocalModuleRoots(tool gotool.Tool, module gotool.Module) ([]string, error) {
	environment, err := tool.Environment(module.Dir)
	if err != nil {
		return nil, err
	}
	return LocalModuleRootsFromEnvironment(tool, module, environment)
}

// LocalModuleRootsFromEnvironment reuses cached go-env bytes from callers.
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

// rootSet bounds transitive replace walks by canonical module directory.
type rootSet struct {
	// seen prevents cycles from adding the same filesystem module twice.
	seen map[string]bool
	// roots preserves scan order so newly found replacements are followed.
	roots []string
}

// newRootSet creates the mutable root accumulator for one discovery pass.
func newRootSet() *rootSet {
	return &rootSet{seen: make(map[string]bool)}
}

// add validates and records one canonical Go module root.
func (s *rootSet) add(dir string) error {
	root, err := canonicalPath(dir)
	if err != nil {
		return fmt.Errorf("resolving local module root %s: %w", dir, err)
	}
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

// sorted returns roots in deterministic order for hashing and listing.
func (s *rootSet) sorted() []string {
	roots := append([]string(nil), s.roots...)
	sort.Strings(roots)
	return roots
}

// addWorkspaceRoots includes every active go.work module root.
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

// addReplaceRoots follows local filesystem replaces until the set stops growing.
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

// validateModuleRoot protects later go-list and hashing assumptions.
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

// canonicalPath normalizes filesystem roots before equality checks.
func canonicalPath(dir string) (string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	assert.True(filepath.IsAbs(root), "canonical root %s must be absolute", root)
	return root, nil
}

// additionalRoots filters the main module from the sorted local-root list.
func additionalRoots(roots []string, mainRoot string) []string {
	mainRoot, err := canonicalPath(mainRoot)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root != mainRoot {
			out = append(out, root)
		}
	}
	return out
}
