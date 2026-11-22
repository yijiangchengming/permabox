//go:build linux

package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yijiangchengming/permabox/internal/oci"
	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

type routes string

type route struct {
	Runtime string
	Bundle  string
}

func (r routes) lookup(id string) (route, error) {
	dir, err := trustedpath.OpenDir(string(r), false, true)
	if errors.Is(err, os.ErrNotExist) {
		return route{}, nil
	}
	if err != nil {
		return route{}, err
	}
	defer dir.Close()
	return readRoute(dir, id)
}

// validateRuntimes runs with create admission locked. Deletes and GC may still
// remove routes, which readRoute treats as absent.
func (r routes) validateRuntimes(root *os.File, check func(string) error) error {
	dir, err := trustedpath.OpenDirIn(root, string(r), false, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".runtime")
		if !ok || !oci.ValidID(id) {
			continue
		}
		rec, err := readRoute(dir, id)
		if err == nil && rec.Runtime != "" {
			err = check(rec.Runtime)
		}
		if err != nil {
			return fmt.Errorf("pinned runtime for %s: %w", id, err)
		}
	}
	return nil
}

func readRoute(dir *os.File, id string) (route, error) {
	fd, err := syscall.Openat(int(dir.Fd()), id+".runtime", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return route{}, nil
	}
	if err != nil {
		return route{}, err
	}
	file := os.NewFile(uintptr(fd), id+".runtime")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return route{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		return route{}, errors.New("runtime route must be a trusted regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return route{}, err
	}
	var rec route
	if err := json.Unmarshal(data, &rec); err != nil {
		return route{}, err
	}
	if !filepath.IsAbs(rec.Runtime) || !filepath.IsAbs(rec.Bundle) {
		return route{}, errors.New("invalid pinned runtime record")
	}
	return rec, nil
}

func (r routes) pin(id, runtime, bundle string) (bool, error) {
	bundle, err := filepath.Abs(bundle)
	if err != nil {
		return false, err
	}
	lock, err := r.lock(true)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	want := route{runtime, bundle}
	got, err := readRoute(lock, id)
	if err != nil {
		return false, err
	}
	if got.Runtime != "" {
		if got != want {
			return false, fmt.Errorf("container %s already pins a different runtime or bundle", id)
		}
		return false, nil
	}
	dir := fmt.Sprintf("/proc/self/fd/%d", lock.Fd())
	f, err := os.CreateTemp(dir, ".runtime-")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if err := errors.Join(json.NewEncoder(f).Encode(want), f.Close()); err != nil {
		return false, err
	}
	err = os.Rename(f.Name(), filepath.Join(dir, id+".runtime"))
	return err == nil, err
}

func (r routes) forget(id string) error {
	lock, err := r.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	return removeRoute(lock, id)
}

func removeRoute(dir *os.File, id string) error {
	err := syscall.Unlinkat(int(dir.Fd()), id+".runtime")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (r routes) sweep(now time.Time, managed func(string) (bool, error)) error {
	lock, err := r.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	entries, err := lock.ReadDir(-1)
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".runtime")
		if !ok || !oci.ValidID(id) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < 5*time.Minute {
			continue
		}
		if live, err := managed(id); err != nil || live {
			result = errors.Join(result, err)
			continue
		}
		rec, err := readRoute(lock, id)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if _, err := os.Lstat(rec.Bundle); errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, removeRoute(lock, id))
		}
	}
	return result
}

func (r routes) lock(create bool) (*os.File, error) {
	f, err := trustedpath.OpenDir(string(r), create, true)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
