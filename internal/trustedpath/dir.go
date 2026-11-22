//go:build linux

package trustedpath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var ErrUnsafeDir = errors.New("unsafe directory")

// FDPath keeps path-based operations relative to an open directory.
func FDPath(dir *os.File) string {
	return fmt.Sprintf("/proc/self/fd/%d", dir.Fd())
}

// OpenDir checks each path component. Private directories must be owned by
// the effective user and must deny access to group and other users.
func OpenDir(path string, create, private bool) (*os.File, error) {
	if path == "/" && private {
		return nil, errors.New("directory cannot be /")
	}
	root, err := os.Open("/")
	if err != nil || path == "/" {
		return root, err
	}
	defer root.Close()
	return OpenDirIn(root, path, create, private)
}

// OpenDirIn checks the components of an absolute path under an open root.
func OpenDirIn(root *os.File, path string, create, private bool) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("directory must be a clean absolute path below the root")
	}
	parent := root
	if parentPath := filepath.Dir(path); parentPath != "/" {
		var err error
		parent, err = OpenDirIn(root, parentPath, create, false)
		if err != nil {
			return nil, err
		}
		defer parent.Close()
	}
	return OpenChild(parent, filepath.Base(path), create, private)
}

// OpenChild opens a single directory under a previously validated parent.
func OpenChild(parent *os.File, name string, create, private bool) (*os.File, error) {
	if !filepath.IsLocal(name) || filepath.Base(name) != name || name == "." {
		return nil, fmt.Errorf("%w name %q", ErrUnsafeDir, name)
	}
	path := filepath.Join(parent.Name(), name)
	flags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	fd, err := syscall.Openat(int(parent.Fd()), name, flags, 0)
	if create && errors.Is(err, os.ErrNotExist) {
		err = syscall.Mkdirat(int(parent.Fd()), name, 0700)
		if err == nil || errors.Is(err, os.ErrExist) {
			fd, err = syscall.Openat(int(parent.Fd()), name, flags, 0)
		}
	}
	if err != nil {
		if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%w component %s: %v", ErrUnsafeDir, path, err)
		}
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), path)
	info, err := dir.Stat()
	if err != nil {
		dir.Close()
		return nil, err
	}
	if !trustedDir(info) || (private && (ownerUID(info) != os.Geteuid() || info.Mode().Perm()&0077 != 0)) {
		dir.Close()
		return nil, fmt.Errorf("%w component %s (uid %d, mode %04o)", ErrUnsafeDir, path, ownerUID(info), info.Mode().Perm())
	}
	return dir, nil
}

func ownerUID(info os.FileInfo) int {
	return int(info.Sys().(*syscall.Stat_t).Uid)
}

func trustedOwner(info os.FileInfo) bool {
	uid := ownerUID(info)
	return uid == 0 || uid == os.Geteuid()
}

func trustedDir(info os.FileInfo) bool {
	mode := info.Mode()
	// A trusted sticky directory protects entries owned by trusted users.
	return info.IsDir() && trustedOwner(info) && (mode.Perm()&0022 == 0 || mode&os.ModeSticky != 0)
}
