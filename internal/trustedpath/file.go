//go:build linux

package trustedpath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// OpenRegularFile opens the verified inode after resolving trusted symlinks.
func OpenRegularFile(path string) (*os.File, error) {
	resolved, expected, err := ResolveFile(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	actual, err := checkedRegular(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	if !os.SameFile(actual, expected) {
		file.Close()
		return nil, fmt.Errorf("untrusted regular file at %s", path)
	}
	return file, nil
}

// OpenRegularAt opens a trusted regular file directly in a verified directory.
func OpenRegularAt(dir *os.File, name string, flags int) (*os.File, error) {
	if !filepath.IsLocal(name) || filepath.Base(name) != name || name == "." {
		return nil, fmt.Errorf("invalid file name %q", name)
	}
	fd, err := syscall.Openat(int(dir.Fd()), name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := checkedRegular(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func checkedRegular(file *os.File) (os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !trustedRegular(info) {
		return nil, fmt.Errorf("untrusted regular file at %s", file.Name())
	}
	return info, nil
}

func trustedRegular(info os.FileInfo) bool {
	return info.Mode().IsRegular() && trustedOwner(info) && info.Mode().Perm()&0022 == 0
}

// ResolveFile checks ownership at each step, including links, and returns
// a canonical regular file that only a trusted user can modify or replace.
func ResolveFile(path string) (string, os.FileInfo, error) {
	return resolveFile("/", path)
}

// ResolveFileIn resolves a path using root as the filesystem root. Absolute
// symlinks are interpreted inside root and the result is an absolute path
// inside that filesystem.
func ResolveFileIn(dir *os.File, path string) (string, os.FileInfo, error) {
	return resolveFile(FDPath(dir), path)
}

func resolveFile(root, path string) (string, os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return "", nil, errors.New("file path must be absolute")
	}
	resolved := "/"
	parts := strings.Split(path, "/")
	links := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, part)
		onDisk := filepath.Join(root, next)
		info, err := os.Lstat(onDisk)
		if err != nil {
			return "", nil, err
		}
		if !trustedOwner(info) {
			return "", nil, fmt.Errorf("untrusted owner of path component %s", next)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", nil, fmt.Errorf("too many symbolic links in %s", path)
			}
			target, err := os.Readlink(onDisk)
			if err != nil {
				return "", nil, err
			}
			if filepath.IsAbs(target) {
				resolved = "/"
			}
			parts = append(strings.Split(target, "/"), parts...)
			continue
		}
		if len(parts) == 0 {
			if !trustedRegular(info) {
				return "", nil, fmt.Errorf("%s must be a regular file writable only by its trusted owner (mode %04o, uid %d)", next, info.Mode().Perm(), ownerUID(info))
			}
			return next, info, nil
		}
		if !trustedDir(info) {
			return "", nil, fmt.Errorf("%w component %s (uid %d, mode %04o)", ErrUnsafeDir, next, ownerUID(info), info.Mode().Perm())
		}
		resolved = next
	}
	return "", nil, fmt.Errorf("%s must resolve to a regular file", path)
}
