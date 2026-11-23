//go:build linux

package oci

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

// ReplaceBundleRoot atomically updates root.path in the existing runtime bundle.
// The caller serializes updates and records both paths before changing the file.
func ReplaceBundleRoot(bundle, from, to string) (published bool, err error) {
	dir, err := openBundle(bundle)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	data, info, err := readConfig(dir)
	if err != nil {
		return false, err
	}
	var spec, root map[string]json.RawMessage
	if err := json.Unmarshal(data, &spec); err != nil {
		return false, err
	}
	if err := json.Unmarshal(spec["root"], &root); err != nil {
		return false, err
	}
	var current string
	if err := json.Unmarshal(root["path"], &current); err != nil {
		return false, err
	}
	if current == to {
		return true, dir.Sync()
	}
	if current != from {
		return false, errors.New("OCI root.path changed since PermaBox prepared the mount")
	}
	root["path"], err = json.Marshal(to)
	if err != nil {
		return false, err
	}
	spec["root"], err = json.Marshal(root)
	if err != nil {
		return false, err
	}
	data, err = json.Marshal(spec)
	if err != nil {
		return false, err
	}
	if len(data) > MaxBundleConfigSize {
		return false, errors.New("redirected OCI config exceeds 4 MiB")
	}
	bundleFD := trustedpath.FDPath(dir)
	temp, err := os.CreateTemp(bundleFD, ".permabox-config-")
	if err != nil {
		return false, err
	}
	defer os.Remove(temp.Name())
	stat := info.Sys().(*syscall.Stat_t)
	if err := temp.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
		temp.Close()
		return false, err
	}
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		temp.Close()
		return false, err
	}
	_, writeErr := temp.Write(data)
	if err := errors.Join(writeErr, temp.Sync(), temp.Close()); err != nil {
		return false, err
	}
	path := filepath.Join(bundleFD, "config.json")
	original, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !os.SameFile(info, original) {
		return false, errors.New("OCI config was replaced during root redirect")
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return false, err
	}
	return true, dir.Sync()
}
