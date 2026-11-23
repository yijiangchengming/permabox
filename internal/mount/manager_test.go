package mount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLifecycle(t *testing.T) {
	m, sys, root, slot := fixture(t)
	fresh, err := m.Prepare("old-id", root, slot, "app")
	if err != nil || !fresh {
		t.Fatalf("prepare: %v, %v", fresh, err)
	}
	if fresh, err := m.Prepare("old-id", root, slot, "app"); fresh || err != nil {
		t.Fatalf("retry: %v, %v", fresh, err)
	}
	if _, err := m.Prepare("new-id", root, slot, "app"); err == nil {
		t.Fatal("two IDs may not share a live upper")
	}
	value := filepath.Join(slot, "upper", "payload")
	if err := os.WriteFile(value, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Cleanup("old-id"); err != nil {
			t.Fatal(err)
		}
	}
	if len(sys.table) != 1 || sys.table[0].ID != 1 || sys.detached {
		t.Fatalf("cleanup damaged base or used lazy detach: %+v", sys)
	}
	if got, err := os.ReadFile(value); err != nil || string(got) != "keep" {
		t.Fatalf("upper lost: %q, %v", got, err)
	}
	if _, err := m.Prepare("new-id", root, slot, "sidecar"); err == nil {
		t.Fatal("reused another container's upper")
	}
	if fresh, err := m.Prepare("new-id", root, slot, "app"); !fresh || err != nil {
		t.Fatalf("recreate: %v, %v", fresh, err)
	}
}

func TestPrepareRejectsSymlinkedStorageSlot(t *testing.T) {
	m, sys, root, slot := fixture(t)
	mkdir(t, filepath.Dir(slot))
	outside := t.TempDir()
	if err := os.Symlink(outside, slot); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare("id", root, slot, "app"); err == nil {
		t.Error("accepted a symlinked storage slot")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 || len(sys.table) != 1 {
		t.Fatalf("prepare changed external directory or mounts: %v, %v, %v", entries, err, sys.table)
	}
}

func TestPrepareRejectsWritableWorkDirectory(t *testing.T) {
	m, _, root, slot := fixture(t)
	work := filepath.Join(slot, "work")
	mkdir(t, work)
	if err := os.Chmod(work, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare("id", root, slot, "app"); err == nil {
		t.Fatal("accepted a writable OverlayFS work directory")
	}
}

func TestPrepareRejectsAliasedLiveSlot(t *testing.T) {
	m, _, root, slot := fixture(t)
	mkdir(t, m.Dir)
	mkdir(t, slot)
	alias := filepath.Join(t.TempDir(), "old-slot")
	if err := os.Symlink(slot, alias); err != nil {
		t.Fatal(err)
	}
	previous := record{Namespace: "mnt:[1]", Root: filepath.Join(filepath.Dir(root), "old-root"), Slot: alias}
	if err := writeRecord(m.journal("previous"), previous); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare("replacement", root, slot, "app"); err == nil {
		t.Fatal("accepted a second journal for the same storage directory")
	}
	assertMissing(t, filepath.Join(slot, "container.json"))
	assertMissing(t, m.journal("replacement"))
}

func TestPrepareReusesLargeContainerMarker(t *testing.T) {
	m, _, root, slot := fixture(t)
	mkdir(t, slot)
	name := strings.Repeat("a", 4096)
	if err := writeRecord(filepath.Join(slot, "container.json"), name); err != nil {
		t.Fatal(err)
	}
	if owned, err := ownedSlot(slot); err != nil || !owned {
		t.Errorf("legacy slot ownership = %v, %v", owned, err)
	}
	if _, err := m.Prepare("id", root, slot, name); err != nil {
		t.Fatalf("reuse legacy slot: %v", err)
	}
}

func TestPrepareRetryChecksOtherLiveSlots(t *testing.T) {
	m, _, root, slot := fixture(t)
	if _, err := m.Prepare("original", root, slot, "app"); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "old-slot")
	if err := os.Symlink(slot, alias); err != nil {
		t.Fatal(err)
	}
	other := record{Namespace: "mnt:[1]", Root: filepath.Join(filepath.Dir(root), "old-root"), Slot: alias}
	if err := writeRecord(m.journal("other"), other); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(slot, releasedMarker)
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare("original", root, slot, "app"); err == nil {
		t.Fatal("retried a container with a conflicting storage journal")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve" {
		t.Fatalf("release marker changed: %q, %v", data, err)
	}
}

func TestCleanupRetainsBusyMount(t *testing.T) {
	m, sys, root, slot := fixture(t)
	if _, err := m.Prepare("id", root, slot, "app"); err != nil {
		t.Fatal(err)
	}
	sys.busy = true
	if err := m.Cleanup("id"); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("cleanup error = %v; want EBUSY", err)
	}
	if live, err := m.Managed("id"); !live || err != nil || len(sys.table) != 3 || sys.detached {
		t.Fatalf("busy cleanup changed the journal or mounts: %v %v %+v", live, err, sys)
	}
}

func TestCleanupRejectsReplacedStorageSlot(t *testing.T) {
	m, _, root, slot := fixture(t)
	if _, err := m.Prepare("id", root, slot, "app"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(slot, slot+"-original"); err != nil {
		t.Fatal(err)
	}
	mkdir(t, slot)
	if err := writeRecord(filepath.Join(slot, "container.json"), "app"); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup("id"); err == nil {
		t.Fatal("accepted a replacement storage directory")
	}
	assertMissing(t, filepath.Join(slot, releasedMarker))
	if _, err := os.Stat(m.journal("id")); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackAndForeignMount(t *testing.T) {
	m, sys, root, slot := fixture(t)
	sys.fail = true
	if _, err := m.Prepare("id", root, slot, "app"); err == nil {
		t.Fatal("mount failure lost")
	}
	if exists, err := m.Managed("id"); exists || err != nil || len(sys.table) != 1 {
		t.Fatalf("rollback leftovers: %v %v %+v", exists, err, sys.table)
	}
	sys.fail = false
	if _, err := m.Prepare("id", root, slot, "app"); err != nil {
		t.Fatal(err)
	}
	top := sys.table[len(sys.table)-1]
	sys.table = append(sys.table, entry{ID: 99, Parent: top.ID, Target: root, Kind: "tmpfs"})
	if err := m.Cleanup("id"); err == nil {
		t.Fatal("cleaned up under a foreign mount")
	}
	if len(sys.table) != 4 {
		t.Fatalf("changed foreign mount stack: %+v", sys.table)
	}
}

func TestCleanupRejectsMountOnCoveredRoot(t *testing.T) {
	m, sys, root, slot := fixture(t)
	if _, err := m.Prepare("id", root, slot, "app"); err != nil {
		t.Fatal(err)
	}
	// A mount can appear at the same path via a separately mounted ancestor.
	sys.table = append(sys.table, entry{ID: 99, Parent: 98, Target: root, Kind: "tmpfs"})
	if err := m.Cleanup("id"); err == nil {
		t.Fatal("cleanup accepted a foreign mount at the same target")
	}
	if len(sys.table) != 4 {
		t.Fatalf("changed foreign mount stack: %+v", sys.table)
	}
}

func TestCleanupRejectsReplacedLower(t *testing.T) {
	m, sys, root, slot := fixture(t)
	if _, err := m.Prepare("id", root, slot, "app"); err != nil {
		t.Fatal(err)
	}
	for i := range sys.table {
		if sys.table[i].Target == m.alias("id") {
			sys.table[i].ID = 99
		}
	}
	if err := m.Cleanup("id"); err == nil || len(sys.table) != 3 {
		t.Fatalf("cleanup changed a replaced lower mount: %v, %+v", err, sys.table)
	}
}

func fixture(t *testing.T) (Manager, *fakeSystem, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "rootfs")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	sys := &fakeSystem{table: []entry{{ID: 1, Target: root, Kind: "overlay"}}}
	return Manager{Dir: filepath.Join(base, "state"), sys: sys}, sys, root, filepath.Join(base, "store", "namespace", "pod")
}

func (m Manager) journal(id string) string { return filepath.Join(m.Dir, id+".json") }

type fakeSystem struct {
	table                []entry
	fail, busy, detached bool
	busyTarget           string
	serial               int
}

func (*fakeSystem) namespace() (string, error) { return "mnt:[1]", nil }
func (f *fakeSystem) mounts() ([]entry, error) { return append([]entry(nil), f.table...), nil }
func (f *fakeSystem) mount(source, target, kind string, flags uintptr, opts string) error {
	if flags&(syscall.MS_REMOUNT|syscall.MS_PRIVATE) != 0 {
		return nil
	}
	if f.fail && kind == "overlay" {
		return errors.New("overlay failed")
	}
	f.serial++
	var parent int
	for _, e := range f.table {
		if e.Target == target {
			parent = e.ID
		}
	}
	f.table = append(f.table, entry{ID: f.serial + 1, Parent: parent, Target: target, Kind: kind, Options: opts})
	return nil
}
func (f *fakeSystem) unmount(path string, flags int) error {
	if (f.busy || path == f.busyTarget) && flags == 0 {
		return syscall.EBUSY
	}
	if flags == syscall.MNT_DETACH {
		f.detached = true
	}
	for i := len(f.table) - 1; i >= 0; i-- {
		if f.table[i].Target == path {
			f.table = append(f.table[:i], f.table[i+1:]...)
			return nil
		}
	}
	return syscall.EINVAL
}
