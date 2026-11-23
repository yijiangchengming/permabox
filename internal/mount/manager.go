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
	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

type Manager struct {
	Dir string
	sys system
}

type lockedManager struct {
	Manager
	*os.File
}

func New(dir string) Manager { return Manager{Dir: dir, sys: linux{}} }

type record struct {
	Namespace       string           `json:"mount_namespace"`
	Root            string           `json:"root"`
	Source          string           `json:"source,omitempty"`
	ConfigRoot      string           `json:"config_root,omitempty"`
	Bundle          string           `json:"bundle,omitempty"`
	SourceID        int              `json:"source_mount_id,omitempty"`
	ConfigPending   bool             `json:"config_redirect_pending,omitempty"`
	Slot            string           `json:"slot"`
	SlotIdentity    *slotIdentity    `json:"slot_identity,omitempty"`
	LowerID         int              `json:"lower_mount_id,omitempty"`
	OverlayID       int              `json:"overlay_mount_id,omitempty"`
	ReleasedAt      time.Time        `json:"released_at,omitempty"`
	PendingOwner    *ownership.Owner `json:"pending_workload_owner,omitempty"`
	ExclusionMounts []exclusionMount `json:"exclusion_mounts,omitempty"`
	Started         bool             `json:"ownership_commit_pending,omitempty"`
}

type exclusionMount struct {
	Path    string `json:"path"`
	Scratch string `json:"scratch"`
	MountID int    `json:"mount_id,omitempty"`
}

type slotIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func (r record) MarshalJSON() ([]byte, error) {
	type fields record
	value := struct {
		fields
		ReleasedAt *time.Time `json:"released_at,omitempty"`
	}{fields: fields(r)}
	if !r.ReleasedAt.IsZero() {
		value.ReleasedAt = &r.ReleasedAt
	}
	return json.Marshal(value)
}

const releasedMarker = ".permabox-released.json"
const maxMountRecordSize = 64 << 10

func (m lockedManager) journal(id string) string {
	return filepath.Join(trustedpath.FDPath(m.File), id+".json")
}

func (m Manager) alias(id string) string { return filepath.Join(m.Dir, id+".lower") }

// Prepare returns true only when this call installed a new overlay.
func (m Manager) Prepare(id, root, slot, container string) (bool, error) {
	return m.PrepareOwned(id, root, slot, container, ownership.Owner{})
}

func (m Manager) PrepareOwned(id, root, slot, container string, owner ownership.Owner) (bool, error) {
	return m.prepare(id, root, "", "", slot, container, owner, false)
}

func (m Manager) PrepareRedirected(id, source, bundle, configRoot, slot, container string, owner ownership.Owner, requireOverlay bool) (bool, error) {
	bundle, err := filepath.Abs(bundle)
	if err != nil {
		return false, err
	}
	return m.prepare(id, source, bundle, configRoot, slot, container, owner, requireOverlay)
}

func (m Manager) prepare(id, root, bundle, configRoot, slot, container string, owner ownership.Owner, requireOverlay bool) (bool, error) {
	if !oci.ValidID(id) {
		return false, errors.New("invalid container ID")
	}
	if len(container) > maxContainerRecordSize {
		return false, errors.New("container name exceeds the storage marker limit")
	}
	containerRecord, err := json.Marshal(container)
	if err != nil {
		return false, err
	}
	if len(containerRecord) > maxContainerRecordSize {
		return false, errors.New("encoded container name exceeds the storage marker limit")
	}
	encodedOwner, err := json.Marshal(owner)
	if err != nil {
		return false, err
	}
	if len(encodedOwner) > maxMountRecordSize {
		return false, errors.New("persistence metadata exceeds the mount record limit")
	}
	inputRoot := root
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return false, err
	}
	if !info.IsDir() || !filepath.IsAbs(root) || !filepath.IsAbs(slot) || overlap(root, slot) || overlap(root, m.Dir) || overlap(slot, m.Dir) {
		return false, errors.New("rootfs, storage slot and state directory must be separate absolute directories")
	}
	exclusions, err := resolveExclusionDirectories(root, owner.Excludes)
	if err != nil {
		return false, err
	}
	owner.Excludes = make([]string, len(exclusions))
	for i, path := range exclusions {
		owner.Excludes[i] = string(filepath.Separator) + path
	}
	lock, err := m.lock(true)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	var source string
	var sourceID int
	if bundle != "" {
		if overlap(bundle, m.Dir) || overlap(bundle, slot) || within(root, bundle) {
			return false, errors.New("OCI bundle overlaps rootfs, storage slot or state directory")
		}
		if requireOverlay {
			if root != inputRoot || overlap(bundle, root) {
				return false, errors.New("CRI-O rootfs must be a separate, non-symlinked directory")
			}
			sourceID, err = m.overlaySourceID(root)
			if err != nil {
				return false, err
			}
		}
		source, root = root, m.redirectTarget(id)
	}
	if isRetiredPath(slot) {
		return false, errors.New("reserved storage slot path")
	}
	slotDir, err := openStorageSlot(slot, true)
	if err != nil {
		return false, err
	}
	defer slotDir.Close()
	slotInfo, err := slotDir.Stat()
	if err != nil {
		return false, err
	}
	alias := m.alias(id)
	if strings.ContainsAny(alias+slot, ",:\\\n\r\t\x00") {
		return false, errors.New("state and storage paths contain characters unsupported by overlay mount options")
	}
	ns, err := m.sys.namespace()
	if err != nil {
		return false, err
	}
	identity := storageIdentity(slotInfo)
	exclusionMounts := make([]exclusionMount, len(exclusions))
	for i, path := range exclusions {
		exclusionMounts[i] = exclusionMount{Path: path, Scratch: filepath.Join(m.Dir, id+".exclude-"+exclusionHash(path))}
	}
	want := record{Namespace: ns, Root: root, Source: source, Bundle: bundle, ConfigRoot: configRoot, SourceID: sourceID, Slot: slot, SlotIdentity: &identity, PendingOwner: &owner, ExclusionMounts: exclusionMounts}
	encodedWant, err := json.Marshal(want)
	if err != nil {
		return false, err
	}
	if len(encodedWant)+len(exclusionMounts)*32 > maxMountRecordSize {
		return false, errors.New("persistence metadata exceeds the mount record limit")
	}
	prev, err := readRecord(lock.journal(id))
	retry := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if retry {
		// A containerd create interrupted before an upgrade can finish using its
		// existing in-place mount. New containers always use the separate rootfs.
		if source != "" && !requireOverlay && prev.Source == "" && prev.Root == source {
			root, source, bundle = source, "", ""
		}
		if prev.Namespace != ns || prev.Root != root || prev.Source != source || prev.Bundle != bundle || prev.SourceID != sourceID || prev.Slot != slot {
			return false, errors.New("container ID already has a different persistence record")
		}
		if prev.SlotIdentity != nil && *prev.SlotIdentity != identity {
			return false, errors.New("storage directory has changed since the previous create")
		}
		if !prev.ReleasedAt.IsZero() {
			return false, errors.New("previous delete still needs cleanup")
		}
	}
	journals, err := lock.ReadDir(-1)
	if err != nil {
		return false, err
	}
	for _, file := range journals {
		if file.Name() == id+".json" || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		other, err := readRecord(filepath.Join(trustedpath.FDPath(lock.File), file.Name()))
		if err != nil {
			return false, err
		}
		if other.Slot == slot || (other.Namespace == ns && (other.Root == root ||
			(source != "" && (other.Source == source || other.Root == source)) || other.Source == root)) {
			return false, fmt.Errorf("rootfs or storage slot already in use by %s", file.Name())
		}
		if other.SlotIdentity != nil {
			if *other.SlotIdentity == identity {
				return false, fmt.Errorf("storage slot already in use through a different path by %s", file.Name())
			}
			continue
		}
		if other.Namespace != ns {
			return false, fmt.Errorf("cannot verify the storage identity of legacy journal %s from a different mount namespace", file.Name())
		}
		otherSlot, err := os.Stat(other.Slot)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect storage slot for %s: %w", file.Name(), err)
		}
		if err == nil && os.SameFile(slotInfo, otherSlot) {
			return false, fmt.Errorf("storage slot already in use through a different path by %s", file.Name())
		}
	}
	if retry {
		mounts, err := m.sys.mounts()
		if err != nil {
			return false, err
		}
		own, err := findOverlay(mounts, prev, alias)
		if err != nil {
			return false, err
		}
		if own == 0 {
			return false, errors.New("previous create was interrupted; delete the container before retrying")
		}
		if source != "" {
			lower, err := lowerMountID(mounts, alias)
			if err != nil {
				return false, err
			}
			if lower == 0 || lower != prev.LowerID || (sourceID == 0 && !sameFile(source, alias)) {
				return false, errors.New("previous create is missing its original lower bind mount")
			}
		}
		if !sameExclusionPaths(prev.ExclusionMounts, exclusions) {
			return false, errors.New("container retry has different persistent directory exclusions")
		}
		for _, exclusion := range prev.ExclusionMounts {
			if id, err := findExclusionOverlay(mounts, prev, alias, exclusion); err != nil || id == 0 {
				if err != nil {
					return false, err
				}
				return false, errors.New("previous create is missing a temporary exclusion overlay")
			}
		}
		if err := bindSlotOwner(slot, owner); err != nil {
			return false, err
		}
		if bundle != "" {
			if err := lock.publishBundleRoot(id, &prev); err != nil {
				return false, err
			}
		}
		return false, removeDurable(filepath.Join(slot, releasedMarker))
	}
	if _, err := os.Lstat(alias); !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("lower mount path is not available: %s", alias)
	}
	if bundle != "" {
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("redirected rootfs path is not available: %s", root)
		}
	}
	fresh := false
	if owner.Valid() {
		var previous slotUse
		entries, err := os.ReadDir(slot)
		if err != nil {
			return false, err
		}
		markerErr := readStorageRecord(filepath.Join(slot, useMarker), &previous)
		if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return false, fmt.Errorf("read writable layer use marker: %w", markerErr)
		}
		fresh = len(entries) == 0 || (markerErr == nil && previous.Fresh && !previous.Confirmed)
	}
	if err := bindSlotOwner(slot, owner); err != nil {
		return false, err
	}
	if err := prepareSlot(slotDir, container, containerRecord, info); err != nil {
		return false, err
	}
	if err := removeDurable(filepath.Join(slot, releasedMarker)); err != nil {
		return false, err
	}
	if owner.Valid() {
		if err := beginSlotUse(slot, id, fresh); err != nil {
			return false, err
		}
	}
	if err := removeDurable(filepath.Join(slot, scaleDownMarker)); err != nil {
		return false, err
	}
	if err := writeRecord(lock.journal(id), want); err != nil {
		return false, err
	}
	err = lock.install(id, &want)
	if err == nil && bundle != "" {
		err = lock.publishBundleRoot(id, &want)
	}
	if err != nil {
		if fresh {
			return false, errors.Join(err, lock.releaseLocked(id, want, time.Now().UTC()))
		}
		return false, errors.Join(err, lock.cleanupLockedAt(id, want, time.Time{}))
	}
	return true, nil
}

// ActivateOwnership commits the pending owner after the runtime starts successfully.
func (m Manager) ActivateOwnership(id string) error {
	if !oci.ValidID(id) {
		return errors.New("invalid container ID")
	}
	lock, err := m.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := readRecord(lock.journal(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !state.ReleasedAt.IsZero() || state.PendingOwner == nil || !state.PendingOwner.Valid() {
		return nil
	}
	state.Started = true
	if err := writeRecord(lock.journal(id), state); err != nil {
		return err
	}
	return lock.commitOwnership(id, state)
}

func (m lockedManager) commitOwnership(id string, state record) error {
	if !state.Started || state.PendingOwner == nil || !state.PendingOwner.Valid() {
		return nil
	}
	slot, err := openRecordedSlot(state)
	if err != nil {
		return err
	}
	defer slot.Close()
	return m.commitSlotOwnership(id, state, trustedpath.FDPath(slot))
}

func (m lockedManager) commitSlotOwnership(id string, state record, slot string) error {
	if !state.Started || state.PendingOwner == nil || !state.PendingOwner.Valid() {
		return nil
	}
	owner := *state.PendingOwner
	owner.Overrides = ownership.Overrides{}
	owner.Excludes = nil
	if err := confirmSlotUse(slot, id, owner); err != nil {
		return err
	}
	state.Started = false
	state.PendingOwner = nil
	return writeRecord(m.journal(id), state)
}

func (m lockedManager) install(id string, state *record) error {
	imageDirectories := make([]os.FileInfo, len(state.ExclusionMounts))
	for i, exclusion := range state.ExclusionMounts {
		info, err := os.Stat(filepath.Join(state.lowerSource(), exclusion.Path))
		if err != nil {
			return err
		}
		imageDirectories[i] = info
	}
	alias := m.alias(id)
	if err := os.Mkdir(alias, 0700); err != nil {
		return err
	}
	if state.Source != "" {
		if err := os.Mkdir(state.Root, 0700); err != nil {
			return err
		}
	}
	if err := m.sys.mount(state.lowerSource(), alias, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind original rootfs: %w", err)
	}
	mounts, err := m.sys.mounts()
	if err != nil {
		return err
	}
	state.LowerID, err = lowerMountID(mounts, alias)
	if err != nil {
		return err
	}
	if state.LowerID == 0 {
		return errors.New("could not find lower bind mount in mountinfo")
	}
	if err := writeRecord(m.journal(id), state); err != nil {
		return err
	}
	// A bind of a shared snapshot inherits its peer group unless isolated here.
	if err := m.sys.mount("", alias, "", syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make lower alias private: %w", err)
	}
	if err := m.sys.mount("", alias, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("remount lower alias read-only: %w", err)
	}
	if err := m.sys.mount("overlay", state.Root, "overlay", 0, overlayOptions(*state, alias)+",index=off,metacopy=off"); err != nil {
		return fmt.Errorf("mount writable overlay: %w", err)
	}
	if state.Source != "" {
		if err := m.sys.mount("", state.Root, "", syscall.MS_PRIVATE, ""); err != nil {
			return fmt.Errorf("make writable overlay private: %w", err)
		}
	}
	mounts, err = m.sys.mounts()
	if err != nil {
		return err
	}
	state.OverlayID, err = findOverlay(mounts, *state, alias)
	if err != nil {
		return err
	}
	if state.OverlayID == 0 {
		return errors.New("could not find writable overlay in mountinfo")
	}
	if err := writeRecord(m.journal(id), state); err != nil {
		return err
	}
	if state.PendingOwner != nil {
		for i := range state.ExclusionMounts {
			if err := m.installExclusion(id, state, alias, i, imageDirectories[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameExclusionPaths(mounts []exclusionMount, paths []string) bool {
	if len(mounts) != len(paths) {
		return false
	}
	for i, path := range paths {
		if mounts[i].Path != path {
			return false
		}
	}
	return true
}

func (m lockedManager) installExclusion(id string, state *record, alias string, index int, imageDirectory os.FileInfo) error {
	item := state.ExclusionMounts[index]
	path := item.Path
	if strings.ContainsAny(item.Scratch+filepath.Join(state.Root, path)+filepath.Join(alias, path), ",:\\\n\r\t\x00") {
		return errors.New("exclusion paths contain characters unsupported by OverlayFS mount options")
	}
	target := filepath.Join(state.Root, path)
	if err := realDir(target); err != nil {
		return fmt.Errorf("exclusion target %s is not a real directory in the writable rootfs: %w", target, err)
	}
	if err := createExclusionScratch(item.Scratch, imageDirectory); err != nil {
		return err
	}
	if err := m.sys.mount("overlay", target, "overlay", 0, exclusionOverlayOptions(alias, item)); err != nil {
		return fmt.Errorf("mount temporary exclusion overlay at %s: %w", target, err)
	}
	if err := m.sys.mount("", target, "", syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make temporary exclusion overlay private at %s: %w", target, err)
	}
	mounts, err := m.sys.mounts()
	if err != nil {
		return err
	}
	item.MountID, err = findExclusionOverlay(mounts, *state, alias, item)
	if err != nil {
		return err
	}
	if item.MountID == 0 {
		return errors.New("could not find temporary exclusion overlay mount")
	}
	state.ExclusionMounts[index] = item
	return writeRecord(m.journal(id), state)
}

func overlayOptions(state record, alias string) string {
	return "lowerdir=" + alias + ",upperdir=" + filepath.Join(state.Slot, "upper") + ",workdir=" + filepath.Join(state.Slot, "work")
}

func findExclusionOverlay(mounts []entry, state record, alias string, item exclusionMount) (int, error) {
	target := filepath.Join(state.Root, item.Path)
	expected := exclusionOverlayLayers(alias, item)
	found := 0
	for _, mount := range mounts {
		if mount.Target != target {
			continue
		}
		if mount.Kind != "overlay" || !hasLayers(mount.Options, expected) {
			return 0, fmt.Errorf("unexpected mount %d (%s, %s) covers excluded directory %s; expected %s", mount.ID, mount.Kind, mount.Options, target, expected)
		}
		if found != 0 {
			return 0, fmt.Errorf("multiple temporary overlays cover excluded directory %s", target)
		}
		if item.MountID != 0 && item.MountID != mount.ID {
			return 0, fmt.Errorf("exclusion overlay mount %d does not match recorded mount %d", mount.ID, item.MountID)
		}
		found = mount.ID
	}
	return found, nil
}

func prepareSlot(dir *os.File, container string, containerRecord []byte, root os.FileInfo) error {
	slot := dir.Name()
	owner := filepath.Join(slot, "container.json")
	saved, err := readContainerRecord(owner)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeRecordData(owner, containerRecord); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if saved != container {
		return fmt.Errorf("storage slot belongs to container %q, not %q", saved, container)
	}
	work, err := trustedpath.OpenChild(dir, "work", true, true)
	if err != nil {
		return err
	}
	defer work.Close()
	upper := filepath.Join(slot, "upper")
	if err := initUpper(upper, root); err != nil {
		return err
	}
	info, err := os.Lstat(upper)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", upper)
	}
	return nil
}

func initUpper(path string, root os.FileInfo) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(path), ".upper-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	// OverlayFS presents the mode and ownership of the upper root inode.
	st := root.Sys().(*syscall.Stat_t)
	if err := os.Chown(tmp, int(st.Uid), int(st.Gid)); err != nil {
		return err
	}
	if err := os.Chmod(tmp, root.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m Manager) Managed(id string) (bool, error) {
	if !oci.ValidID(id) {
		return false, errors.New("invalid container ID")
	}
	dir, err := trustedpath.OpenDir(m.Dir, false, true)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer dir.Close()
	info, err := os.Lstat(filepath.Join(trustedpath.FDPath(dir), id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err == nil && !info.Mode().IsRegular() {
		return false, errors.New("invalid persistence journal")
	}
	return err == nil, err
}

func (m Manager) Cleanup(id string) error {
	_, err := m.CleanupSlot(id)
	return err
}

// CleanupSlot returns the released slot after successful unmount and cleanup.
func (m Manager) CleanupSlot(id string) (string, error) {
	if !oci.ValidID(id) {
		return "", errors.New("invalid container ID")
	}
	lock, err := m.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer lock.Close()
	state, err := readRecord(lock.journal(id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := lock.releaseLocked(id, state, time.Now().UTC()); err != nil {
		return "", err
	}
	return state.Slot, nil
}

func (m lockedManager) releaseLocked(id string, state record, now time.Time) error {
	if state.ReleasedAt.IsZero() {
		state.ReleasedAt = now
		if err := writeRecord(m.journal(id), state); err != nil {
			return err
		}
	} else if err := m.Sync(); err != nil {
		return err
	}
	return m.cleanupLockedAt(id, state, now)
}

func (m lockedManager) cleanupLockedAt(id string, state record, now time.Time) error {
	ns, err := m.sys.namespace()
	if err != nil {
		return err
	}
	if ns != state.Namespace {
		return errors.New("mount namespace differs from create; persistence record retained")
	}
	if state.Source != "" && state.Root != m.redirectTarget(id) {
		return errors.New("invalid redirected rootfs target in journal")
	}
	var slot *os.File
	if !now.IsZero() {
		slot, err = openRecordedSlot(state)
		if err != nil {
			needsOwner := state.Started && state.PendingOwner != nil && state.PendingOwner.Valid()
			if state.SlotIdentity != nil || needsOwner || (!errors.Is(err, os.ErrNotExist) && !errors.Is(err, trustedpath.ErrUnsafeDir)) {
				return err
			}
		} else {
			defer slot.Close()
		}
	}
	alias := m.alias(id)
	mounts, err := m.sys.mounts()
	if err != nil {
		return err
	}
	own, err := findOverlay(mounts, state, alias)
	if err != nil {
		return err
	}
	lower, err := lowerMountID(mounts, alias)
	if err != nil {
		return err
	}
	for _, e := range mounts {
		if (state.OverlayID != 0 && e.ID == state.OverlayID && e.ID != own) ||
			(state.LowerID != 0 && e.ID == state.LowerID && e.ID != lower) {
			return errors.New("recorded mount ID no longer identifies its expected mount")
		}
	}
	for _, target := range []struct {
		path string
		id   int
	}{{state.Root, own}, {alias, lower}} {
		if target.id != 0 {
			if err := realDir(target.path); err != nil {
				return fmt.Errorf("unsafe mount target %s: %w", target.path, err)
			}
		}
	}
	if lower != 0 && lower != state.LowerID {
		// A crash immediately after bind can leave a mount without a recorded ID.
		// Before the overlay is installed, the two paths should name the same inode.
		if state.LowerID != 0 || own != 0 || !sameFile(state.lowerSource(), alias) {
			return errors.New("lower alias is covered by a mount not owned by PermaBox")
		}
	}
	if state.Source != "" {
		for _, e := range mounts {
			if e.Target == state.Root && e.ID != own {
				return errors.New("redirected rootfs target has a foreign mount")
			}
		}
		if state.ConfigPending {
			if _, err := oci.ReplaceBundleRoot(state.Bundle, state.Root, state.originalConfigRoot()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("restore OCI bundle root: %w", err)
			}
		}
	}
	for i := len(state.ExclusionMounts) - 1; i >= 0; i-- {
		exclusion := state.ExclusionMounts[i]
		target := filepath.Join(state.Root, exclusion.Path)
		id, err := findExclusionOverlay(mounts, state, alias, exclusion)
		if err != nil {
			return err
		}
		if id != 0 {
			if err := realDir(target); err != nil {
				return fmt.Errorf("unsafe exclusion mount target %s: %w", target, err)
			}
			if err := m.sys.unmount(target, 0); err != nil {
				return fmt.Errorf("unmount temporary exclusion overlay at %s: %w", target, err)
			}
		}
	}
	if len(state.ExclusionMounts) > 0 {
		mounts, err = m.sys.mounts()
		if err != nil {
			return err
		}
		for _, exclusion := range state.ExclusionMounts {
			if mountReferences(mounts, exclusion.Scratch) {
				return fmt.Errorf("temporary exclusion storage %s is still referenced by a mount", exclusion.Scratch)
			}
			if err := removeExclusionScratch(m.Dir, id, exclusion); err != nil {
				return err
			}
		}
	}
	if own != 0 {
		if err := m.sys.unmount(state.Root, 0); err != nil {
			return fmt.Errorf("unmount writable overlay: %w", err)
		}
	}
	if lower != 0 {
		if err := m.sys.unmount(alias, 0); err != nil {
			return fmt.Errorf("unmount lower alias: %w", err)
		}
	}
	if err := remove(alias); err != nil {
		return err
	}
	if state.Source != "" {
		if err := remove(state.Root); err != nil {
			return err
		}
	}
	if slot != nil {
		path := trustedpath.FDPath(slot)
		if err := m.commitSlotOwnership(id, state, path); err != nil {
			return err
		}
		if _, err := readContainerRecord(filepath.Join(path, "container.json")); err == nil {
			if err := writeRecord(filepath.Join(path, releasedMarker), now); err != nil {
				return err
			}
		}
	}
	return removeDurable(m.journal(id))
}

func sameFile(a, b string) bool {
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && os.SameFile(left, right)
}

func lowerMountID(mounts []entry, alias string) (int, error) {
	id := 0
	for _, e := range mounts {
		if e.Target == alias {
			if id != 0 {
				return 0, errors.New("unexpected mount stack on the private lower alias")
			}
			id = e.ID
		}
	}
	return id, nil
}

func findOverlay(mounts []entry, state record, alias string) (int, error) {
	var own *entry
	for i := range mounts {
		e := &mounts[i]
		if e.Target == state.Root && e.Kind == "overlay" && hasLayers(e.Options, overlayOptions(state, alias)) {
			if own != nil {
				return 0, fmt.Errorf("multiple matching rootfs overlays: mounts %d and %d", own.ID, e.ID)
			}
			if state.OverlayID != 0 && state.OverlayID != e.ID {
				return 0, fmt.Errorf("rootfs overlay mount %d does not match recorded mount %d", e.ID, state.OverlayID)
			}
			own = e
		}
	}
	if own == nil {
		return 0, nil
	}
	parents := make(map[int]int, len(mounts))
	for _, e := range mounts {
		parents[e.ID] = e.Parent
	}
	ancestors := make(map[int]bool)
	for id := own.Parent; id != 0; id = parents[id] {
		if id == own.ID || ancestors[id] {
			return 0, errors.New("invalid mount ancestry")
		}
		ancestors[id] = true
	}
	for _, e := range mounts {
		if e.Target == state.Root && e.ID != own.ID && !ancestors[e.ID] {
			return 0, errors.New("a separate mount may cover the PermaBox overlay")
		}
	}
	return own.ID, nil
}

func hasLayers(options, expected string) bool {
	present := "," + options + ","
	for _, layer := range strings.Split(expected, ",") {
		if !strings.Contains(present, ","+layer+",") {
			return false
		}
	}
	return true
}

func (m Manager) lock(create bool) (*lockedManager, error) {
	f, err := trustedpath.OpenDir(m.Dir, create, true)
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
	return &lockedManager{Manager: m, File: f}, nil
}

func overlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || a == "/" || b == "/" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func readRecord(path string) (record, error) {
	var out record
	if err := readRecordFile(path, maxMountRecordSize, &out); err != nil {
		return out, err
	}
	if out.Namespace == "" || !filepath.IsAbs(out.Root) || !filepath.IsAbs(out.Slot) ||
		filepath.Clean(out.Root) != out.Root || filepath.Clean(out.Slot) != out.Slot {
		return out, fmt.Errorf("incomplete persistence record %s", path)
	}
	if out.Source != "" || out.Bundle != "" || out.ConfigRoot != "" || out.SourceID != 0 || out.ConfigPending {
		if !filepath.IsAbs(out.Source) || !filepath.IsAbs(out.Bundle) || out.SourceID < 0 ||
			filepath.Clean(out.Source) != out.Source || filepath.Clean(out.Bundle) != out.Bundle ||
			overlap(out.Source, out.Root) || overlap(out.Bundle, out.Root) || within(out.Source, out.Bundle) {
			return out, fmt.Errorf("invalid redirected rootfs record %s", path)
		}
	}
	for i, exclusion := range out.ExclusionMounts {
		if !filepath.IsLocal(exclusion.Path) || filepath.Clean(exclusion.Path) != exclusion.Path || exclusion.Path == "." ||
			strings.ContainsAny(exclusion.Path, ",:\\\n\r\t\x00") || !filepath.IsAbs(exclusion.Scratch) ||
			filepath.Clean(exclusion.Scratch) != exclusion.Scratch || exclusion.MountID < 0 {
			return out, fmt.Errorf("invalid exclusion mount record %s", path)
		}
		for _, prior := range out.ExclusionMounts[:i] {
			if overlap(prior.Path, exclusion.Path) {
				return out, fmt.Errorf("overlapping exclusion mount records %s", path)
			}
		}
	}
	return out, nil
}

func writeRecord(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeRecordData(path, data)
}

func writeRecordData(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".record-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func removeDurable(path string) error {
	if err := remove(path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func remove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
