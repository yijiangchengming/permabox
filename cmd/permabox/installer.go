//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yijiangchengming/permabox/internal/config"
	"github.com/yijiangchengming/permabox/internal/lifecycle"
	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

func preparePrivateDirectory(path string) (*os.File, error) {
	dir, err := trustedpath.OpenDir(path, true, false)
	if err != nil {
		return nil, err
	}
	info, err := dir.Stat()
	if err != nil {
		dir.Close()
		return nil, err
	}
	uid := uint32(os.Geteuid())
	if info.Sys().(*syscall.Stat_t).Uid != uid || info.Mode().Perm()&0022 != 0 {
		dir.Close()
		return nil, fmt.Errorf("directory %s must be writable only by the installer user", path)
	}
	if info.Mode() != os.ModeDir|0700 {
		if err := dir.Chmod(0700); err != nil {
			dir.Close()
			return nil, err
		}
	}
	return dir, nil
}

func prepareInstallerState(path string) error {
	dir, err := preparePrivateDirectory(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	uid := uint32(os.Geteuid())
	for _, name := range []string{"lock", "create.lock"} {
		lock, err := os.Lstat(filepath.Join(trustedpath.FDPath(dir), name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !lock.Mode().IsRegular() || lock.Mode().Perm()&0022 != 0 || lock.Sys().(*syscall.Stat_t).Uid != uid {
			return fmt.Errorf("unsafe installer lock %s (mode %s, uid %d)", name, lock.Mode(), lock.Sys().(*syscall.Stat_t).Uid)
		}
	}
	transaction, err := trustedpath.OpenChild(dir, ".transaction", false, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return transaction.Close()
}

func prepareBridgeState(path, owner string) error {
	dir, err := preparePrivateDirectory(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Keep release ownership authoritative in the installer state directory.
	// This copy coordinates bridge Pods that share the same socket directory.
	base := trustedpath.FDPath(dir)
	file, err := os.CreateTemp(base, ".owner-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = fmt.Fprintln(file, owner)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(base, "owner"))
}

func checkBridgeMarker(path, expected string) error {
	dir, err := trustedpath.OpenDir(filepath.Dir(path), false, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := trustedpath.OpenRegularAt(dir, filepath.Base(path), syscall.O_RDONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	expected += "\n"
	data, err := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	if err != nil {
		return err
	}
	if string(data) != expected {
		return errors.New("workload bridge marker does not match")
	}
	return nil
}

func openHostRoot(root string) (*os.File, error) {
	if root == "/proc/1/root" {
		return os.Open(root)
	}
	return trustedpath.OpenDir(root, false, false)
}

func printHostExecutable(root, path string) error {
	dir, err := openHostRoot(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	resolved, info, err := trustedpath.ResolveFileIn(dir, path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("host binary %s is not executable", path)
	}
	if strings.ContainsAny(resolved, "\r\n") {
		return fmt.Errorf("host binary %s resolves to a path containing a line break", path)
	}
	_, err = fmt.Fprintln(os.Stdout, resolved)
	return err
}

func validateHostRuntimes(root, wrapper string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir, err := openHostRoot(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return lifecycle.ValidateNodeRuntimes(cfg, dir, wrapper)
}

func validateContainerdDirectory(path string) error {
	dir, err := openContainerdDirectory(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("containerd directory %s is writable by non-owners", path)
	}
	for _, name := range []string{"config.toml", "config.toml.tmpl", "config-v3.toml.tmpl"} {
		file, err := trustedpath.OpenRegularAt(dir, name, syscall.O_RDONLY)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("validate containerd file %s: %w", name, err)
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func openContainerdDirectory(path string) (*os.File, error) {
	const hostRoot = "/proc/1/root"
	relative, hostPath := strings.CutPrefix(path, hostRoot+"/")
	if !hostPath {
		return trustedpath.OpenDir(path, false, false)
	}
	dir, err := openHostRoot(hostRoot)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return trustedpath.OpenDirIn(dir, "/"+relative, false, false)
}

func validateInstalledRuntime(dir string) error {
	for _, name := range []string{"runtime", "permabox", "config.yaml"} {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		file, err := trustedpath.OpenRegularFile(path)
		if err != nil {
			return fmt.Errorf("validate installed %s: %w", name, err)
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func verifyRuntimeCache(path, digest string, prepare bool) error {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("invalid runtime generation digest")
	}
	versions, err := trustedpath.OpenDir(path, prepare, true)
	if err != nil {
		return err
	}
	defer versions.Close()
	version, err := trustedpath.OpenChild(versions, digest, false, true)
	if prepare && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer version.Close()
	generation := sha256.New()
	for _, name := range []string{"permabox", "config.yaml"} {
		sum, err := runtimeFileDigest(version, name)
		if err != nil {
			return fmt.Errorf("check cached runtime %s: %w", digest, err)
		}
		fmt.Fprintf(generation, "%x\n", sum)
	}
	if hex.EncodeToString(generation.Sum(nil)) != digest {
		return fmt.Errorf("cached runtime generation %s does not match the expected digest", digest)
	}
	return nil
}

func runtimeFileDigest(version *os.File, name string) ([]byte, error) {
	file, err := trustedpath.OpenRegularAt(version, name, syscall.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if name == "permabox" {
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0111 == 0 {
			return nil, errors.New("cached PermaBox binary is not executable")
		}
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
}
