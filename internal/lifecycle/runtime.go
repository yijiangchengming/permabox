//go:build linux

package lifecycle

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yijiangchengming/permabox/internal/config"
	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

func checkRuntime(path string) (string, error) {
	resolved, target, err := trustedpath.ResolveFile(path)
	if err != nil {
		return "", err
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return "", err
	}
	return resolved, validateRuntimeExecutable(path, target, self)
}

func validateRuntimeExecutable(path string, target os.FileInfo, permaboxFiles ...os.FileInfo) error {
	if target.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("underlying runtime %s is not executable", path)
	}
	for _, file := range permaboxFiles {
		if os.SameFile(target, file) {
			return fmt.Errorf("underlying runtime %s points to PermaBox", path)
		}
	}
	return nil
}

// ValidateNodeRuntimes checks the configured and pinned executables in the host
// root before publishing a K3s generation. The caller holds create admission.
func ValidateNodeRuntimes(cfg config.Config, root *os.File, wrapper string) error {
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return err
	}
	permaboxFiles := []os.FileInfo{self}
	installDir := filepath.Dir(wrapper)
	binary := filepath.Join(installDir, "permabox")
	versions := filepath.Join(installDir, "versions") + "/"
	for _, path := range []string{wrapper, binary} {
		_, info, err := trustedpath.ResolveFileIn(root, path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		permaboxFiles = append(permaboxFiles, info)
	}
	checked := make(map[string]struct{})
	check := func(path string) error {
		if _, ok := checked[path]; ok {
			return nil
		}
		resolved, target, err := trustedpath.ResolveFileIn(root, path)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", path, err)
		}
		if resolved == wrapper || resolved == binary ||
			(strings.HasPrefix(resolved, versions) && filepath.Base(resolved) == "permabox") {
			return fmt.Errorf("underlying runtime %s points to PermaBox", path)
		}
		if err := validateRuntimeExecutable(path, target, permaboxFiles...); err != nil {
			return err
		}
		checked[path] = struct{}{}
		return nil
	}
	if err := check(cfg.Runtime); err != nil {
		return fmt.Errorf("default runtime: %w", err)
	}
	for _, alias := range slices.Sorted(maps.Keys(cfg.UnderlyingRuntimes)) {
		if err := check(cfg.UnderlyingRuntimes[alias]); err != nil {
			return fmt.Errorf("runtime alias %q: %w", alias, err)
		}
	}
	return routes(cfg.StateDir).validateRuntimes(root, check)
}
