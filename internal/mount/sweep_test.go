//go:build linux

package mount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yijiangchengming/permabox/internal/ownership"
)

func TestSweepWorkloadProtectsReusedAndUntrackedSlots(t *testing.T) {
	m, _, root, slot := fixture(t)
	store := filepath.Dir(filepath.Dir(slot))
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "app", UID: "app-uid"}
	if _, err := m.PrepareOwned("first", root, slot, "app", owner); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateOwnership("first"); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup("first"); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(filepath.Dir(slot), "legacy")
	prepareReleasedSlot(t, legacy, time.Now())
	roots := []string{store}
	candidates, err := m.DiscoverReleased(roots, "", 0, time.Now())
	if err != nil || len(candidates) != 1 || candidates[0].Owner() != owner {
		t.Fatalf("discover = %+v, %v", candidates, err)
	}
	// Recreate and release the same slot while the policy is being evaluated.
	if _, err := m.PrepareOwned("second", root, slot, "app", owner); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup("second"); err != nil {
		t.Fatal(err)
	}
	if err := m.RetireReleased(roots, "", time.Now(), candidates); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(slot); err != nil {
		t.Fatal(err)
	}
	candidates, err = m.DiscoverReleased(roots, "", 0, time.Now())
	if err != nil || len(candidates) != 1 || candidates[0].Owner() != owner {
		t.Fatalf("rediscover = %+v, %v", candidates, err)
	}
	if err := m.RetireReleased(roots, "", time.Now(), candidates); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, slot)
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal(err)
	}
}

func TestFailedCreatePreservesOwner(t *testing.T) {
	m, _, root, slot := fixture(t)
	roots := []string{filepath.Dir(filepath.Dir(slot))}
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "app", UID: "original"}
	if _, err := m.PrepareOwned("first", root, slot, "app", owner); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateOwnership("first"); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup("first"); err != nil {
		t.Fatal(err)
	}
	replacement := owner
	replacement.UID = "replacement"
	if _, err := m.PrepareOwned("failed-create", root, slot, "app", replacement); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup("failed-create"); err != nil {
		t.Fatal(err)
	}
	candidates, err := m.DiscoverReleased(roots, "", 0, time.Now())
	if err != nil || len(candidates) != 1 || candidates[0].Owner() != owner {
		t.Fatalf("discover after failed create = %+v, %v", candidates, err)
	}
}

func TestSweepRetriesTerminalJournal(t *testing.T) {
	m, sys, root, slot := fixture(t)
	if _, err := m.Prepare("id", root, slot, "app"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.journal("id"))
	if err != nil || strings.Contains(string(data), "released_at") {
		t.Fatalf("active journal: %s, %v", data, err)
	}
	// Busy lower bind exercises retry after the overlay has already unmounted.
	sys.busyTarget = m.alias("id")
	if err := m.Cleanup("id"); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("cleanup = %v", err)
	}
	state, err := readRecord(m.journal("id"))
	if err != nil || state.ReleasedAt.IsZero() || len(sys.table) != 2 || sys.detached {
		t.Fatalf("journal = %+v, %v; mounts = %+v", state, err, sys)
	}
	marker := filepath.Join(slot, releasedMarker)
	assertMissing(t, marker)
	if _, err := m.Prepare("id", root, slot, "app"); err == nil {
		t.Fatal("Prepare accepted a terminal journal")
	}
	now := state.ReleasedAt.Add(time.Hour)
	if err := m.Sweep(nil, 0, now); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("sweep while busy = %v", err)
	}
	assertMissing(t, marker)
	sys.busyTarget = ""
	if err := m.Sweep(nil, 0, now); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, m.journal("id"))
	assertMissing(t, m.alias("id"))
	var released time.Time
	if !readMarker(marker, &released) || !released.Equal(now) {
		t.Fatalf("release marker = %v; want %v", released, now)
	}
	if fresh, err := m.Prepare("next", root, slot, "app"); err != nil || !fresh {
		t.Fatalf("reuse = %v, %v", fresh, err)
	}
	assertMissing(t, marker)
}

func TestSweepStaleJournals(t *testing.T) {
	for _, scenario := range []struct {
		name                                                        string
		keepBundle, young, foreignNS, movedID, mountedRoot, symlink bool
	}{
		{name: "removed"},
		{name: "bundle exists", keepBundle: true},
		{name: "young", young: true},
		{name: "foreign namespace", foreignNS: true},
		{name: "moved mount ID", movedID: true},
		{name: "mount at root", mountedRoot: true},
		{name: "dangling bundle symlink", symlink: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, sys, _, slot := fixture(t)
			mkdir(t, m.Dir)
			bundle := filepath.Join(filepath.Dir(m.Dir), "bundle")
			root := filepath.Join(bundle, "rootfs")
			ns, _ := sys.namespace()
			state := record{Namespace: ns, Root: root, Slot: slot, LowerID: 17}
			mkdir(t, m.alias("id"))
			prepareReleasedSlot(t, slot, time.Time{})
			sys.table = []entry{{ID: 17, Target: m.alias("id")}}
			if scenario.keepBundle {
				mkdir(t, bundle)
			}
			if scenario.foreignNS {
				state.Namespace = "mnt:[55]"
			}
			if scenario.movedID {
				sys.table[0].Target = bundle + "-other"
			}
			if scenario.mountedRoot {
				sys.table = append(sys.table, entry{ID: 99, Target: root})
			}
			if scenario.symlink {
				if err := os.Symlink(bundle+"-missing", bundle); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeRecord(m.journal("id"), state); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if !scenario.young {
				old := now.Add(-6 * time.Minute)
				if err := os.Chtimes(m.journal("id"), old, old); err != nil {
					t.Fatal(err)
				}
			}
			err := m.Sweep(nil, 0, now)
			if (err != nil) != scenario.movedID {
				t.Fatalf("sweep = %v", err)
			}
			if scenario.name == "removed" {
				assertMissing(t, m.journal("id"))
				assertMissing(t, m.alias("id"))
				if len(sys.table) != 0 {
					t.Fatalf("orphan lower mount retained: %+v", sys.table)
				}
			} else if got, err := readRecord(m.journal("id")); err != nil || !got.ReleasedAt.IsZero() || len(sys.table) == 0 {
				t.Fatalf("unsafe stale reclaim: %+v, %v; mounts: %+v", got, err, sys.table)
			}
		})
	}
}

func TestSweepReleasedSlots(t *testing.T) {
	m, sys, root, slot := fixture(t)
	store, namespace := filepath.Dir(filepath.Dir(slot)), filepath.Dir(slot)
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	if _, err := m.Prepare("live", root, slot, "app"); err != nil {
		t.Fatal(err)
	}
	// A live slot stays protected even if it has an old marker and the
	// journal belongs to a different mount namespace from the sweep.
	state, err := readRecord(m.journal("live"))
	if err != nil {
		t.Fatal(err)
	}
	state.Namespace = "mnt:[55]"
	if err := writeRecord(m.journal("live"), state); err != nil {
		t.Fatal(err)
	}
	if err := writeRecord(filepath.Join(slot, releasedMarker), old); err != nil {
		t.Fatal(err)
	}
	sys.table = []entry{{ID: 1, Target: "/", Device: "0:42", FSRoot: "/"}}
	for _, name := range []string{"expired", "fresh", "legacy", "descendant", "layer", "bind", "marker-link", "no-owner", retiredPrefix + "leftover"} {
		pod := filepath.Join(namespace, name)
		when := old
		if name == "fresh" {
			when = now
		}
		if name == "legacy" {
			when = time.Time{}
		}
		prepareReleasedSlot(t, pod, when)
	}
	for _, path := range []string{filepath.Join(namespace, "no-owner", "container.json"), filepath.Join(namespace, "marker-link", releasedMarker)} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(filepath.Dir(store), "outside", "namespace", "outside-pod")
	prepareReleasedSlot(t, outside, old)
	for link, target := range map[string]string{
		filepath.Join(store, "linked-namespace"):                filepath.Dir(outside),
		filepath.Join(namespace, "linked-pod"):                  outside,
		filepath.Join(namespace, "marker-link", releasedMarker): filepath.Join(outside, releasedMarker),
		filepath.Join(namespace, "expired", "upper", "outside"): outside,
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	sys.table = append(sys.table,
		entry{ID: 2, Target: filepath.Join(namespace, "descendant", "upper", "other")},
		entry{ID: 3, Target: "/another-root", Kind: "overlay", Options: "lowerdir=/base:/more,upperdir=" + filepath.Join(namespace, "layer", "upper")},
		entry{ID: 4, Target: "/bind-target", Device: "0:42", FSRoot: filepath.Join(namespace, "bind", "upper")},
	)
	if err := m.Sweep([]string{store}, 0, now); err != nil {
		t.Fatal(err)
	}
	check(t, filepath.Join(namespace, "expired", "upper"), "payload", "keep")
	if err := m.Sweep([]string{store}, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, filepath.Join(namespace, "expired"))
	assertMissing(t, filepath.Join(namespace, retiredPrefix+"leftover"))
	for _, name := range []string{filepath.Base(slot), "fresh", "legacy", "descendant", "layer", "bind", "marker-link", "no-owner", "linked-pod"} {
		if name == filepath.Base(slot) {
			continue // The active slot has no test payload.
		}
		check(t, filepath.Join(namespace, name, "upper"), "payload", "keep")
	}
	if _, err := os.Lstat(slot); err != nil {
		t.Fatal(err)
	}
	check(t, filepath.Join(outside, "upper"), "payload", "keep")
}

func TestSweepReleasedSlotsAfterReboot(t *testing.T) {
	m, _, _, slot := fixture(t)
	store := filepath.Dir(filepath.Dir(slot))
	now := time.Now()
	prepareReleasedSlot(t, slot, now.Add(-2*time.Hour))
	assertMissing(t, m.Dir)
	if err := m.Sweep([]string{store}, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, slot)
	if info, err := os.Stat(m.Dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("missing private state directory: %v, %v", info, err)
	}
}

func TestSweepProtectsNestedStorageRoot(t *testing.T) {
	m, _, _, slot := fixture(t)
	store := filepath.Dir(filepath.Dir(slot))
	nested := filepath.Join(slot, "nested-store")
	nestedSlot := filepath.Join(nested, "other", "pod")
	now := time.Now()
	prepareReleasedSlot(t, slot, now.Add(-2*time.Hour))
	prepareReleasedSlot(t, nestedSlot, now)
	if err := m.Sweep([]string{store, nested}, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	check(t, filepath.Join(nestedSlot, "upper"), "payload", "keep")
}

func TestMountReferencesEscapedLayers(t *testing.T) {
	for _, options := range []string{
		`lowerdir=/base:/store/ns/pod\072one/upper`,
		`lowerdir=/base:/store/ns/pod\134:one/upper`,
		`lowerdir+=/store/ns/pod:one/upper`,
		`workdir=/store/ns/pod:one/work`,
		`upperdir=/store/ns/pod\054one/upper`,
	} {
		mounts, err := readMounts(strings.NewReader("1 9 0:42 / /elsewhere rw - overlay overlay rw," + options + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		slot := "/store/ns/pod:one"
		if strings.Contains(options, `\054`) {
			slot = "/store/ns/pod,one"
		}
		if !mountReferences(mounts, slot) {
			t.Errorf("mount options %q did not protect %s", options, slot)
		}
	}
}

func prepareReleasedSlot(t *testing.T, slot string, released time.Time) {
	t.Helper()
	mkdir(t, filepath.Join(slot, "upper"))
	put(t, filepath.Join(slot, "upper"), "payload", "keep")
	if err := writeRecord(filepath.Join(slot, "container.json"), "app"); err != nil {
		t.Fatal(err)
	}
	if !released.IsZero() {
		if err := writeRecord(filepath.Join(slot, releasedMarker), released); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be removed, got %v", path, err)
	}
}
