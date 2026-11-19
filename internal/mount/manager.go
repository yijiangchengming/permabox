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
	"github.com/yijiangchengming/permabox/internal/workload"
)

type Manager struct {
	Dir string
	sys system
}

func New(dir string) Manager { return Manager{Dir: dir, sys: linux{}} }

type record struct {
	Namespace    string          `json:"mount_namespace"`
	Root         string          `json:"root"`
	Slot         string          `json:"slot"`
	LowerID      int             `json:"lower_mount_id,omitempty"`
	OverlayID    int             `json:"overlay_mount_id,omitempty"`
	ReleasedAt   time.Time       `json:"released_at,omitempty"`
	PendingOwner *workload.Owner `json:"pending_workload_owner,omitempty"`
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

func (m Manager) journal(id string) string { return filepath.Join(m.Dir, id+".json") }
func (m Manager) alias(id string) string   { return filepath.Join(m.Dir, id+".lower") }

// Prepare returns true only when this call installed a new overlay.
func (m Manager) Prepare(id, root, slot, container string) (bool, error) {
	return m.PrepareOwned(id, root, slot, container, workload.Owner{})
}

func (m Manager) PrepareOwned(id, root, slot, container string, owner workload.Owner) (bool, error) {
	if !oci.ValidID(id) {
		return false, errors.New("invalid container ID")
	}
	root, err := filepath.EvalSymlinks(root)
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
	lock, err := m.lock(true)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if isRetiredPath(slot) {
		return false, errors.New("reserved storage slot path")
	}
	if err := os.MkdirAll(slot, 0700); err != nil {
		return false, err
	}
	slot, err = filepath.EvalSymlinks(slot)
	if err != nil {
		return false, err
	}
	if overlap(root, slot) || overlap(slot, m.Dir) || isRetiredPath(slot) {
		return false, errors.New("resolved storage slot overlaps runtime directories or uses a reserved path")
	}
	alias := m.alias(id)
	if strings.ContainsAny(alias+slot, ",:\\\n\r\t\x00") {
		return false, errors.New("state and storage paths contain characters unsupported by overlay mount options")
	}
	ns, err := m.sys.namespace()
	if err != nil {
		return false, err
	}
	want := record{Namespace: ns, Root: root, Slot: slot, PendingOwner: &owner}
	prev, err := readRecord(m.journal(id))
	switch {
	case err == nil:
		if prev.Namespace != ns || prev.Root != root || prev.Slot != slot {
			return false, errors.New("container ID already has a different persistence record")
		}
		if !prev.ReleasedAt.IsZero() {
			return false, errors.New("previous delete still needs cleanup")
		}
		mounts, err := m.sys.mounts()
		if err != nil {
			return false, err
		}
		own, err := findOverlay(mounts, prev, alias)
		if err == nil && own == 0 {
			err = errors.New("previous create was interrupted; delete the container before retrying")
		}
		if err == nil {
			err = removeDurable(filepath.Join(slot, releasedMarker))
		}
		return false, err
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	}
	journals, err := os.ReadDir(m.Dir)
	if err != nil {
		return false, err
	}
	for _, file := range journals {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		other, err := readRecord(filepath.Join(m.Dir, file.Name()))
		if err != nil {
			return false, err
		}
		if other.Slot == slot || (other.Namespace == ns && other.Root == root) {
			return false, fmt.Errorf("rootfs or storage slot already in use by %s", file.Name())
		}
	}
	if _, err := os.Lstat(alias); !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("lower mount path is not available: %s", alias)
	}
	if err := prepareSlot(slot, container, info); err != nil {
		return false, err
	}
	if err := removeDurable(filepath.Join(slot, releasedMarker)); err != nil {
		return false, err
	}
	if err := writeRecord(m.journal(id), want); err != nil {
		return false, err
	}
	if err := m.install(id, &want); err != nil {
		return false, errors.Join(err, m.cleanupLocked(id, want))
	}
	return true, nil
}

// ActivateOwnership commits the owner once OCI start confirms create was accepted.
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
	state, err := readRecord(m.journal(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !state.ReleasedAt.IsZero() || state.PendingOwner == nil {
		return nil
	}
	if err := realDir(state.Slot); err != nil {
		return err
	}
	path := filepath.Join(state.Slot, workloadMarker)
	if state.PendingOwner.Valid() {
		return writeRecord(path, state.PendingOwner)
	}
	return removeDurable(path)
}

func (m Manager) install(id string, state *record) error {
	alias := m.alias(id)
	if err := os.Mkdir(alias, 0700); err != nil {
		return err
	}
	if err := m.sys.mount(state.Root, alias, "", syscall.MS_BIND, ""); err != nil {
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
	if err := m.sys.mount("", alias, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("remount lower alias read-only: %w", err)
	}
	if err := m.sys.mount("overlay", state.Root, "overlay", 0, overlayOptions(*state, alias)+",index=off,metacopy=off"); err != nil {
		return fmt.Errorf("mount writable overlay: %w", err)
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
	return writeRecord(m.journal(id), state)
}

func overlayOptions(state record, alias string) string {
	return "lowerdir=" + alias + ",upperdir=" + filepath.Join(state.Slot, "upper") + ",workdir=" + filepath.Join(state.Slot, "work")
}

func prepareSlot(slot, container string, root os.FileInfo) error {
	owner := filepath.Join(slot, "container.json")
	name, err := os.ReadFile(owner)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeRecord(owner, container); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		var saved string
		if err := json.Unmarshal(name, &saved); err != nil {
			return err
		}
		if saved != container {
			return fmt.Errorf("storage slot belongs to container %q, not %q", saved, container)
		}
	}
	upper, work := filepath.Join(slot, "upper"), filepath.Join(slot, "work")
	if err := initUpper(upper, root); err != nil {
		return err
	}
	if err := os.Mkdir(work, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	for _, dir := range []string{upper, work} {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("not a directory: %s", dir)
		}
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
	_, err := os.Stat(m.journal(id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
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
	state, err := readRecord(m.journal(id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := m.releaseLocked(id, state, time.Now().UTC()); err != nil {
		return "", err
	}
	return state.Slot, nil
}

func (m Manager) releaseLocked(id string, state record, now time.Time) error {
	if state.ReleasedAt.IsZero() {
		state.ReleasedAt = now
		if err := writeRecord(m.journal(id), state); err != nil {
			return err
		}
	} else if err := syncDir(m.Dir); err != nil {
		return err
	}
	return m.cleanupLockedAt(id, state, now)
}

func (m Manager) cleanupLocked(id string, state record) error {
	return m.cleanupLockedAt(id, state, time.Time{}) // Interrupted create rollback.
}

func (m Manager) cleanupLockedAt(id string, state record, now time.Time) error {
	ns, err := m.sys.namespace()
	if err != nil {
		return err
	}
	if ns != state.Namespace {
		return errors.New("mount namespace differs from create; persistence record retained")
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
		if state.LowerID != 0 || own != 0 || !sameFile(state.Root, alias) {
			return errors.New("lower alias is covered by a mount not owned by PermaBox")
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
	if !now.IsZero() {
		owned, err := ownedSlot(state.Slot)
		if err != nil {
			return err
		}
		if owned {
			if err := writeRecord(filepath.Join(state.Slot, releasedMarker), now); err != nil {
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

func (m Manager) lock(create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(m.Dir, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(m.Dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state directory must be private (0700)")
	}
	f, err := os.Open(m.Dir)
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

func overlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || a == "/" || b == "/" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func readRecord(path string) (record, error) {
	var out record
	data, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("decode %s: %w", path, err)
	}
	if out.Namespace == "" || !filepath.IsAbs(out.Root) || !filepath.IsAbs(out.Slot) ||
		filepath.Clean(out.Root) != out.Root || filepath.Clean(out.Slot) != out.Slot {
		return out, fmt.Errorf("incomplete persistence record %s", path)
	}
	return out, nil
}

func writeRecord(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
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
