//go:build linux

package mount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yijiangchengming/permabox/internal/oci"
	"github.com/yijiangchengming/permabox/internal/ownership"
	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

const (
	staleAge        = 5 * time.Minute
	retiredPrefix   = ".permabox-retired-"
	workloadMarker  = ".permabox-workload.json"
	scaleDownMarker = ".permabox-scale-down.json"
	scaleDownGrace  = time.Minute
)

type ReclaimReason uint8

const (
	ReclaimNone ReclaimReason = iota
	ReclaimZero
	ReclaimDeleted
)

// ReleasedSlot identifies one release generation eligible for a policy check.
type ReleasedSlot struct {
	path     string
	owner    ownership.Owner
	at       time.Time
	file     os.FileInfo
	identity slotIdentity
}

func (s ReleasedSlot) Owner() ownership.Owner { return s.owner }

type slotFilter func(ReleasedSlot) bool

// DiscoverReleased collects policy candidates while performing the regular TTL sweep.
// An empty target selects all storage slots.
func (m Manager) DiscoverReleased(roots []string, target string, ttl time.Duration, now time.Time) ([]ReleasedSlot, error) {
	var candidates []ReleasedSlot
	err := m.sweep(roots, target, ttl, now, time.Time{}, func(s ReleasedSlot) bool {
		candidates = append(candidates, s)
		return false
	})
	return candidates, err
}

// RetireReleased rechecks the approved release generations under the mount lock.
func (m Manager) RetireReleased(roots []string, target string, now time.Time, approved []ReleasedSlot) error {
	return m.retireApproved(roots, target, now, approved, 0, time.Time{})
}

// StageScaledDownBefore requires a renewed approval with a short lifetime.
func (m Manager) StageScaledDownBefore(roots []string, target string, now time.Time, approved []ReleasedSlot, generation int64, deadline time.Time) error {
	if generation <= 0 || deadline.IsZero() {
		return errors.New("scale-down approval requires a StatefulSet generation and deadline")
	}
	return m.retireApproved(roots, target, now, approved, generation, deadline)
}

type scaleDown struct {
	Released   time.Time `json:"released"`
	Since      time.Time `json:"since"`
	Generation int64     `json:"generation"`
}

func scaleDownReady(slot ReleasedSlot, now time.Time, generation int64) (bool, error) {
	path := filepath.Join(slot.path, scaleDownMarker)
	var pending scaleDown
	if readMarker(path, &pending) && pending.Generation == generation && pending.Released.Equal(slot.at) && !pending.Since.IsZero() && !now.Before(pending.Since) {
		return now.Sub(pending.Since) >= scaleDownGrace, nil
	}
	return false, writeRecord(path, scaleDown{Released: slot.at, Since: now, Generation: generation})
}

func (m Manager) retireApproved(roots []string, target string, now time.Time, approved []ReleasedSlot, generation int64, deadline time.Time) error {
	if len(approved) == 0 {
		return nil
	}
	byPath := make(map[string]ReleasedSlot, len(approved))
	for _, s := range approved {
		byPath[s.path] = s
	}
	if now.IsZero() {
		now = time.Now()
	}
	var stageErr error
	err := m.sweep(roots, target, 0, now, deadline, func(s ReleasedSlot) bool {
		a, ok := byPath[s.path]
		if !ok || s.identity != a.identity || s.owner != a.owner || !s.at.Equal(a.at) || !os.SameFile(s.file, a.file) {
			return false
		}
		if generation == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		ready, err := scaleDownReady(s, now.UTC(), generation)
		stageErr = errors.Join(stageErr, err)
		return ready && time.Now().Before(deadline)
	})
	return errors.Join(err, stageErr)
}

// Sweep retries released journals and recovers stale records in this mount
// namespace. A positive idleTTL also enables deletion of released slots under
// the supplied storage roots. A failed cleanup leaves its journal for retry.
func (m Manager) Sweep(roots []string, idleTTL time.Duration, now time.Time) error {
	return m.sweep(roots, "", idleTTL, now, time.Time{}, nil)
}

func (m Manager) sweep(roots []string, target string, idleTTL time.Duration, now, deadline time.Time, filter slotFilter) error {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	lock, err := m.lock(len(roots) != 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	retired, sweepErr := lock.sweepLocked(roots, target, idleTTL, now, deadline, filter)
	result := errors.Join(sweepErr, lock.Close())
	for _, slot := range retired {
		if err := m.deleteRetired(slot); err != nil {
			result = errors.Join(result, fmt.Errorf("delete retired slot %s: %w", slot, err))
		}
	}
	return result
}

func (m lockedManager) sweepLocked(roots []string, target string, ttl time.Duration, now, deadline time.Time, filter slotFilter) ([]string, error) {
	ns, err := m.sys.namespace()
	if err != nil {
		return nil, err
	}
	files, err := m.ReadDir(-1)
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
		info, err := os.Lstat(m.journal(id))
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
			(now.Sub(info.ModTime()) >= staleAge && staleRoot(state, m.alias(id), mounts))) {
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
		} else if state.Started && state.Namespace == ns {
			result = errors.Join(result, m.commitOwnership(id, state))
		}
		live = append(live, state)
	}
	if unknown || len(roots) == 0 {
		return nil, result
	}
	mounts, err = m.sys.mounts() // Refresh after unmounting.
	if err != nil {
		return nil, errors.Join(result, err)
	}
	var retired []string
	liveSlots := make(map[slotIdentity]bool)
	for _, state := range live {
		if state.SlotIdentity != nil {
			liveSlots[*state.SlotIdentity] = true
		} else if state.Namespace == ns {
			if info, err := os.Stat(state.Slot); err == nil {
				liveSlots[storageIdentity(info)] = true
			}
		}
	}
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
		paths, err := retireSlots(filepath.Clean(root), target, m.Dir, protectedRoots, live, liveSlots, mounts, ttl, now, deadline, filter)
		retired = append(retired, paths...)
		result = errors.Join(result, err)
	}
	return retired, result
}

func staleRoot(state record, alias string, mounts []entry) bool {
	bundle := state.Bundle
	if bundle == "" {
		bundle = filepath.Dir(state.Root)
	}
	if !errors.Is(realDir(bundle), os.ErrNotExist) {
		return false
	}
	for _, mount := range mounts {
		if within(state.Root, mount.Target) || (state.Source != "" && within(alias, mount.Target)) {
			return false
		}
	}
	return true
}

func retireSlots(root, target, stateDir string, protectedRoots []string, live []record, liveSlots map[slotIdentity]bool, mounts []entry, ttl time.Duration, now, deadline time.Time, filter slotFilter) ([]string, error) {
	if target != "" && filepath.Dir(filepath.Dir(target)) != root {
		return nil, nil
	}
	rootDir, err := storageDir(trustedpath.OpenDir(root, false, false))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, trustedpath.ErrUnsafeDir) {
			return nil, nil
		}
		return nil, err
	}
	defer rootDir.Close()
	namespaces, err := rootDir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var retired []string
	var result error
	currentBoot := sync.OnceValues(bootID)
	for _, namespace := range namespaces {
		if !namespace.IsDir() || strings.HasPrefix(namespace.Name(), ".") {
			continue
		}
		parent := filepath.Join(root, namespace.Name())
		if target != "" && parent != filepath.Dir(target) {
			continue
		}
		parentDir, err := storageDir(trustedpath.OpenChild(rootDir, namespace.Name(), false, false))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, trustedpath.ErrUnsafeDir) {
			continue
		}
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		pods, err := parentDir.ReadDir(-1)
		parentDir.Close()
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		for _, pod := range pods {
			parked := strings.HasPrefix(pod.Name(), retiredPrefix)
			if name, ok := strings.CutSuffix(pod.Name(), receiptSuffix); parked && ok && target == "" && pod.Type().IsRegular() {
				path := filepath.Join(parent, name)
				var receipt retirementReceipt
				if !readMarker(path+receiptSuffix, &receipt) || receipt.Inode == 0 {
					continue
				}
				// A crash before the rename, or after deleting the directory,
				// can leave a receipt with no directory to delete.
				if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
					if err := removeDurable(path + receiptSuffix); err != nil {
						result = errors.Join(result, err)
					}
				}
				continue
			}
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
			if len(liveSlots) != 0 {
				info, err := os.Lstat(slot)
				if err != nil || liveSlots[storageIdentity(info)] {
					continue
				}
			}
			if parked {
				var receipt retirementReceipt
				if readMarker(slot+receiptSuffix, &receipt) {
					if validRetirement(slot, receipt) {
						retired = append(retired, slot)
					}
					continue
				}
				if _, err := os.Lstat(slot + receiptSuffix); !errors.Is(err, os.ErrNotExist) {
					continue
				}
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
			if !parked {
				eligible, err := recoverSlotRelease(slot, now, currentBoot)
				if err != nil {
					result = errors.Join(result, err)
				}
				if !eligible {
					continue
				}
			}
			if !readMarker(marker, &released) || released.IsZero() {
				continue
			}
			if parked {
				if err := recordRetirement(slot, slot); err != nil {
					result = errors.Join(result, err)
					continue
				}
				retired = append(retired, slot)
				continue
			}
			if ttl <= 0 || now.Sub(released) < ttl {
				var owner ownership.Owner
				if filter == nil || !readMarker(filepath.Join(slot, workloadMarker), &owner) || !owner.Valid() || owner.Namespace != namespace.Name() {
					continue
				}
				info, err := os.Lstat(marker)
				if err != nil {
					continue
				}
				directory, err := os.Lstat(slot)
				if err != nil || !directory.IsDir() {
					continue
				}
				if !filter(ReleasedSlot{path: slot, owner: owner, at: released, file: info, identity: storageIdentity(directory)}) {
					continue
				}
			}
			path, err := retireSlot(slot, deadline)
			if err != nil {
				result = errors.Join(result, fmt.Errorf("retire slot %s: %w", slot, err))
				continue
			}
			if path != "" {
				retired = append(retired, path)
			}
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
	dir, err := openStorageSlot(slot, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, trustedpath.ErrUnsafeDir) {
			return false, nil
		}
		return false, err
	}
	defer dir.Close()
	// An empty container name is valid for OCI bundles without a name annotation.
	_, err = readContainerRecord(filepath.Join(slot, "container.json"))
	return err == nil, nil
}

func readMarker(path string, out any) bool {
	return readStorageRecord(path, out) == nil
}

func journalReferences(live []record, slot string) bool {
	for _, state := range live {
		if overlap(slot, state.Slot) || overlap(slot, state.Root) {
			return true
		}
	}
	return false
}

const receiptSuffix = ".receipt"

type retirementReceipt struct {
	Inode  uint64  `json:"inode"`
	Device *uint64 `json:"device,omitempty"`
}

func retirementIdentity(path string) (retirementReceipt, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return retirementReceipt{}, err
	}
	return receiptForDirectory(info)
}

func receiptForDirectory(info os.FileInfo) (retirementReceipt, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0022 != 0 || (stat.Uid != 0 && int(stat.Uid) != os.Geteuid()) {
		return retirementReceipt{}, errUnsafeDir
	}
	device := uint64(stat.Dev)
	return retirementReceipt{Inode: stat.Ino, Device: &device}, nil
}

func validRetirement(slot string, receipt retirementReceipt) bool {
	got, err := retirementIdentity(slot)
	return err == nil && receipt.matches(got)
}

func (receipt retirementReceipt) matches(got retirementReceipt) bool {
	return receipt.Inode == got.Inode && receipt.Device != nil && *receipt.Device == *got.Device
}

func recordRetirement(original, parked string) error {
	receipt, err := retirementIdentity(original)
	if err != nil {
		return err
	}
	return writeRecord(parked+receiptSuffix, receipt)
}

func retireSlot(slot string, deadline time.Time) (string, error) {
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
	if _, err := os.Lstat(retired + receiptSuffix); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("retirement name already has a receipt")
	}
	if err := recordRetirement(slot, retired); err != nil {
		return "", err
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return "", removeDurable(retired + receiptSuffix)
	}
	if err := os.Rename(slot, retired); err != nil {
		return "", errors.Join(err, removeDurable(retired+receiptSuffix))
	}
	return retired, syncDir(parent)
}

func (m Manager) deleteRetired(slot string) error {
	// Pin the parent so recursive deletion cannot be redirected via a symlink.
	parent, err := storageDir(trustedpath.OpenDir(filepath.Dir(slot), false, false))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(slot)
	path := filepath.Join(trustedpath.FDPath(parent), name)
	var receipt retirementReceipt
	if !readMarker(path+receiptSuffix, &receipt) {
		return nil
	}
	dir, err := trustedpath.OpenChild(parent, name, false, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if dir != nil {
		defer dir.Close()
		info, err := dir.Stat()
		if err != nil {
			return err
		}
		got, err := receiptForDirectory(info)
		if err != nil || !receipt.matches(got) {
			return errors.New("retired directory does not match its receipt")
		}
		mounts, err := m.sys.mounts()
		if err != nil {
			return err
		}
		if mountReferences(mounts, slot) {
			return errors.New("retired directory still has a mount reference")
		}
		if err := parent.Sync(); err != nil {
			return err
		}
		mountID, err := directoryMountID(parent)
		if err != nil {
			return err
		}
		if err := removeRetiredContents(dir, mountID); err != nil {
			return err
		}
		current, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !os.SameFile(info, current) {
			return errors.New("retired directory was replaced during deletion")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	if err := remove(path + receiptSuffix); err != nil {
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
