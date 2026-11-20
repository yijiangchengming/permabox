//go:build linux

package mount

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yijiangchengming/permabox/internal/oci"
	"github.com/yijiangchengming/permabox/internal/ownership"
)

const (
	staleAge       = 5 * time.Minute
	retiredPrefix  = ".permabox-retired-"
	workloadMarker = ".permabox-workload.json"
)

// ReleasedSlot identifies one release generation eligible for a policy check.
type ReleasedSlot struct {
	path  string
	owner ownership.Owner
	at    time.Time
	file  os.FileInfo
}

func (s ReleasedSlot) Owner() ownership.Owner { return s.owner }

type slotFilter func(ReleasedSlot) bool

// DiscoverReleased collects policy candidates while performing the regular TTL sweep.
// An empty target selects all storage slots.
func (m Manager) DiscoverReleased(roots []string, target string, ttl time.Duration, now time.Time) ([]ReleasedSlot, error) {
	var candidates []ReleasedSlot
	err := m.sweep(roots, target, ttl, now, func(s ReleasedSlot) bool {
		candidates = append(candidates, s)
		return false
	})
	return candidates, err
}

// RetireReleased rechecks the approved release generations under the mount lock.
func (m Manager) RetireReleased(roots []string, target string, now time.Time, approved []ReleasedSlot) error {
	if len(approved) == 0 {
		return nil
	}
	byPath := make(map[string]ReleasedSlot, len(approved))
	for _, s := range approved {
		byPath[s.path] = s
	}
	return m.sweep(roots, target, 0, now, func(s ReleasedSlot) bool {
		a, ok := byPath[s.path]
		return ok && s.owner == a.owner && s.at.Equal(a.at) && os.SameFile(s.file, a.file)
	})
}

// Sweep retries released journals and recovers stale records in this mount
// namespace. A positive idleTTL also enables deletion of released slots under
// the supplied storage roots. A failed cleanup leaves its journal for retry.
func (m Manager) Sweep(roots []string, idleTTL time.Duration, now time.Time) error {
	return m.sweep(roots, "", idleTTL, now, nil)
}

func (m Manager) sweep(roots []string, target string, idleTTL time.Duration, now time.Time, filter slotFilter) error {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	lock, err := m.lock((idleTTL > 0 || filter != nil) && len(roots) != 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	retired, sweepErr := m.sweepLocked(roots, target, idleTTL, now, filter)
	result := errors.Join(sweepErr, lock.Close())
	for _, slot := range retired {
		if err := deleteRetired(slot); err != nil {
			result = errors.Join(result, fmt.Errorf("delete retired slot %s: %w", slot, err))
		}
	}
	return result
}

func (m Manager) sweepLocked(roots []string, target string, ttl time.Duration, now time.Time, filter slotFilter) ([]string, error) {
	ns, err := m.sys.namespace()
	if err != nil {
		return nil, err
	}
	files, err := os.ReadDir(m.Dir)
	if err != nil {
		return nil, err
	}
	mounts, err := m.sys.mounts()
	if err != nil {
		return nil, err
	}
	var result error
	var live []record
	unknown := false
	for _, file := range files {
		id, ok := strings.CutSuffix(file.Name(), ".json")
		if !ok {
			continue
		}
		info, err := file.Info()
		if err == nil && (!info.Mode().IsRegular() || !oci.ValidID(id)) {
			err = errors.New("invalid journal entry")
		}
		var state record
		if err == nil {
			state, err = readRecord(m.journal(id))
		}
		if err != nil {
			unknown = true
			result = errors.Join(result, fmt.Errorf("inspect %s: %w", file.Name(), err))
			continue
		}
		terminal := !state.ReleasedAt.IsZero()
		if target == "" && state.Namespace == ns && (terminal ||
			(now.Sub(info.ModTime()) >= staleAge && staleRoot(state.Root, mounts))) {
			var err error
			if terminal {
				err = m.releaseLocked(id, state, now)
			} else {
				err = m.cleanupLockedAt(id, state, now)
			}
			if err == nil {
				continue
			}
			result = errors.Join(result, fmt.Errorf("clean up %s: %w", id, err))
		}
		live = append(live, state)
	}
	if (ttl <= 0 && filter == nil) || unknown || len(roots) == 0 {
		return nil, result
	}
	mounts, err = m.sys.mounts() // Refresh after unmounting.
	if err != nil {
		return nil, errors.Join(result, err)
	}
	var retired []string
	protectedRoots := make([]string, 0, len(roots)*2)
	for _, root := range roots {
		if filepath.IsAbs(root) {
			protectedRoots = append(protectedRoots, filepath.Clean(root))
			if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != filepath.Clean(root) {
				protectedRoots = append(protectedRoots, resolved)
			}
		}
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
			result = errors.Join(result, fmt.Errorf("invalid storage root %q", root))
			continue
		}
		paths, err := retireSlots(filepath.Clean(root), target, m.Dir, protectedRoots, live, mounts, ttl, now, filter)
		retired = append(retired, paths...)
		result = errors.Join(result, err)
	}
	return retired, result
}

func staleRoot(root string, mounts []entry) bool {
	// The journal only records rootfs. Require its containing directory to
	// be gone as well, so an existing bundle with a missing rootfs is kept.
	if !errors.Is(realDir(filepath.Dir(root)), os.ErrNotExist) {
		return false
	}
	for _, mount := range mounts {
		if within(root, mount.Target) {
			return false
		}
	}
	return true
}

func retireSlots(root, target, stateDir string, protectedRoots []string, live []record, mounts []entry, ttl time.Duration, now time.Time, filter slotFilter) ([]string, error) {
	if target != "" && filepath.Dir(filepath.Dir(target)) != root {
		return nil, nil
	}
	if err := realDir(root); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errUnsafeDir) {
			return nil, nil
		}
		return nil, err
	}
	namespaces, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var retired []string
	var result error
	for _, namespace := range namespaces {
		if !namespace.IsDir() || strings.HasPrefix(namespace.Name(), ".") {
			continue
		}
		parent := filepath.Join(root, namespace.Name())
		if target != "" && parent != filepath.Dir(target) {
			continue
		}
		pods, err := os.ReadDir(parent)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		for _, pod := range pods {
			parked := strings.HasPrefix(pod.Name(), retiredPrefix)
			if !pod.IsDir() || (strings.HasPrefix(pod.Name(), ".") && !parked) {
				continue
			}
			slot := filepath.Join(parent, pod.Name())
			if target != "" && slot != target {
				continue
			}
			if overlap(slot, stateDir) || containsStorageRoot(slot, protectedRoots) || journalReferences(live, slot) || mountReferences(mounts, slot) {
				continue
			}
			owned, err := ownedSlot(slot)
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			if !owned {
				continue
			}
			marker := filepath.Join(slot, releasedMarker)
			var released time.Time
			if !readMarker(marker, &released) || released.IsZero() {
				continue
			}
			if parked {
				retired = append(retired, slot)
				continue
			}
			if ttl <= 0 || now.Sub(released) < ttl {
				var owner ownership.Owner
				if filter == nil || !readMarker(filepath.Join(slot, workloadMarker), &owner) || !owner.Valid() || owner.Namespace != namespace.Name() {
					continue
				}
				info, err := os.Lstat(marker)
				if err != nil || !filter(ReleasedSlot{path: slot, owner: owner, at: released, file: info}) {
					continue
				}
			}
			path, err := retireSlot(slot)
			if err != nil {
				result = errors.Join(result, fmt.Errorf("retire slot %s: %w", slot, err))
				continue
			}
			retired = append(retired, path)
		}
	}
	return retired, result
}

func containsStorageRoot(slot string, roots []string) bool {
	for _, root := range roots {
		if within(slot, root) {
			return true
		}
	}
	return false
}

func ownedSlot(slot string) (bool, error) {
	if err := realDir(slot); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errUnsafeDir) {
			return false, nil
		}
		return false, err
	}
	// An empty container name is valid for OCI bundles without a name annotation.
	var owner *string
	return readMarker(filepath.Join(slot, "container.json"), &owner) && owner != nil, nil
}

func readMarker(path string, out any) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, out) == nil
}

func journalReferences(live []record, slot string) bool {
	for _, state := range live {
		if overlap(slot, state.Slot) || overlap(slot, state.Root) {
			return true
		}
	}
	return false
}

func retireSlot(slot string) (string, error) {
	parent := filepath.Dir(slot)
	retired, err := os.MkdirTemp(parent, retiredPrefix)
	if err != nil {
		return "", err
	}
	// Some filesystems reject renaming over even an empty directory. The
	// manager lock reserves the hidden name after removing the placeholder.
	if err := os.Remove(retired); err != nil {
		return "", err
	}
	if err := os.Rename(slot, retired); err != nil {
		return "", err
	}
	return retired, syncDir(parent)
}

func deleteRetired(slot string) error {
	// Pin the parent so recursive deletion cannot be redirected via a symlink.
	parent, err := openRealDir(filepath.Dir(slot))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	path := filepath.Join(fmt.Sprintf("/proc/self/fd/%d", parent.Fd()), filepath.Base(slot))
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return parent.Sync()
}

func isRetiredPath(path string) bool {
	for _, part := range strings.Split(filepath.Clean(path), "/") {
		if strings.HasPrefix(part, retiredPrefix) {
			return true
		}
	}
	return false
}

var errUnsafeDir = errors.New("path contains a symlink or non-directory")

func realDir(path string) error {
	dir, err := openRealDir(path)
	if err != nil {
		return err
	}
	return dir.Close()
}

func openRealDir(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errUnsafeDir
	}
	const flags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	fd, err := syscall.Open("/", flags, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		if part == "" {
			continue
		}
		next, err := syscall.Openat(fd, part, flags, 0)
		_ = syscall.Close(fd)
		if err != nil {
			if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
				return nil, errUnsafeDir
			}
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

func within(parent, path string) bool {
	return filepath.IsAbs(path) && (path == parent || strings.HasPrefix(path, strings.TrimSuffix(parent, "/")+"/"))
}
