//go:build linux

package mount

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	if err := m.ActivateOwnership("second"); err != nil {
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
	if _, err := m.PrepareOwned("replacement", root, slot, "app", replacement); err == nil {
		t.Fatal("replacement StatefulSet inherited the old writable layer")
	}
	if _, err := m.PrepareOwned("unverified", root, slot, "app", ownership.Owner{}); err == nil {
		t.Fatal("unverified workload inherited the old writable layer")
	}
	if _, err := m.PrepareOwned("rollout", root, slot, "app", owner); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup("rollout"); err != nil {
		t.Fatal(err)
	}
	candidates, err := m.DiscoverReleased(roots, "", 0, time.Now())
	if err != nil || len(candidates) != 0 {
		t.Fatalf("discover after failed create = %+v, %v", candidates, err)
	}
}

func TestRetirementApprovalRejectsReplacedDirectory(t *testing.T) {
	m, _, _, slot := fixture(t)
	now := time.Now()
	roots := []string{filepath.Dir(filepath.Dir(slot))}
	prepareReleasedSlot(t, slot, now)
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "app", UID: "app-uid"}
	if err := writeRecord(filepath.Join(slot, workloadMarker), owner); err != nil {
		t.Fatal(err)
	}
	approved, err := m.DiscoverReleased(roots, "", 0, now)
	if err != nil || len(approved) != 1 {
		t.Fatalf("discover = %v, %v", approved, err)
	}
	old := slot + "-old"
	if err := os.Rename(slot, old); err != nil {
		t.Fatal(err)
	}
	prepareReleasedSlot(t, slot, now)
	for _, name := range []string{workloadMarker, releasedMarker} {
		if err := os.Rename(filepath.Join(old, name), filepath.Join(slot, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RetireReleased(roots, "", now, approved); err != nil {
		t.Fatal(err)
	}
	check(t, filepath.Join(slot, "upper"), "payload", "keep")
}

func TestIdleTTLReclaimsOnlyEmptyReleasedFirstUseWithoutConfirmation(t *testing.T) {
	for _, scenario := range []struct {
		name                                     string
		payload, priorUse, released, mountFailed bool
	}{
		{name: "empty first use", released: true},
		{name: "mount rollback", mountFailed: true},
		{name: "possible writes before confirmation", payload: true, released: true},
		{name: "previously confirmed use", priorUse: true, released: true},
		{name: "not released"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, sys, root, slot := fixture(t)
			roots := []string{filepath.Dir(filepath.Dir(slot))}
			owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "app", UID: "original"}
			if scenario.priorUse {
				if _, err := m.PrepareOwned("previous", root, slot, "app", owner); err != nil {
					t.Fatal(err)
				}
				if err := m.ActivateOwnership("previous"); err != nil {
					t.Fatal(err)
				}
				if err := m.Cleanup("previous"); err != nil {
					t.Fatal(err)
				}
			}
			sys.fail = scenario.mountFailed
			if _, err := m.PrepareOwned("failed", root, slot, "app", owner); (err != nil) != scenario.mountFailed {
				t.Fatalf("prepare: %v", err)
			}
			if scenario.payload {
				put(t, filepath.Join(slot, "upper"), "payload", "keep")
			}
			if scenario.released {
				if err := m.Cleanup("failed"); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().Add(2 * time.Hour)
			if err := m.Sweep(roots, 0, now); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(slot); err != nil {
				t.Fatal(err)
			}
			if err := m.Sweep(roots, time.Hour, now); err != nil {
				t.Fatal(err)
			}
			if scenario.name == "empty first use" || scenario.mountFailed {
				assertMissing(t, slot)
			} else if _, err := os.Stat(slot); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoverConfirmedUseAfterReboot(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		m, sys, root, slot := fixture(t)
		roots := []string{filepath.Dir(filepath.Dir(slot))}
		owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "app", UID: "original"}
		if _, err := m.PrepareOwned("id", root, slot, "app", owner); err != nil {
			t.Fatal(err)
		}
		if confirmed {
			if err := m.ActivateOwnership("id"); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.RemoveAll(m.Dir); err != nil {
			t.Fatal(err)
		}
		sys.table = nil
		now := time.Now()
		got, err := m.DiscoverReleased(roots, "", 0, now)
		if err != nil || len(got) != 0 {
			t.Fatalf("without boot change: %v, %v", got, err)
		}
		var use slotUse
		path := filepath.Join(slot, useMarker)
		if !readMarker(path, &use) {
			t.Fatal("missing persistent use")
		}
		use.Boot = "old-boot"
		if err := writeRecord(path, use); err != nil {
			t.Fatal(err)
		}
		got, err = m.DiscoverReleased(roots, "", 0, now)
		if err != nil || (len(got) == 1) != confirmed {
			t.Fatalf("after reboot (confirmed=%v): %v, %v", confirmed, got, err)
		}
		if err := m.Sweep(roots, time.Hour, now.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(slot)
		if (err == nil) == confirmed {
			t.Fatalf("after TTL (confirmed=%v): %v", confirmed, err)
		}
	}
}

func TestScaleDownGraceProtectsReuse(t *testing.T) {
	m, _, root, slot := fixture(t)
	roots := []string{filepath.Dir(filepath.Dir(slot))}
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "app", UID: "original"}
	for _, id := range []string{"first", "replacement"} {
		if _, err := m.PrepareOwned(id, root, slot, "app", owner); err != nil {
			t.Fatal(err)
		}
		if err := m.ActivateOwnership(id); err != nil {
			t.Fatal(err)
		}
		if err := m.Cleanup(id); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		got, err := m.DiscoverReleased(roots, "", 0, now)
		if err != nil || len(got) != 1 {
			t.Fatalf("discover: %v, %v", got, err)
		}
		if err := m.StageScaledDownBefore(roots, "", now, got, 1, time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := m.StageScaledDownBefore(roots, "", now.Add(scaleDownGrace-time.Second), got, 1, time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(slot); err != nil {
			t.Fatal(err)
		}
		if id == "replacement" {
			if err := m.StageScaledDownBefore(roots, "", now.Add(scaleDownGrace), got, 2, time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(slot); err != nil {
				t.Fatal("generation change must restart the grace period", err)
			}
			if err := m.StageScaledDownBefore(roots, "", now.Add(2*scaleDownGrace), got, 2, time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			assertMissing(t, slot)
		}
	}
}

func TestExpiredRetirementDoesNotRenameOrLeaveReceipt(t *testing.T) {
	_, _, _, slot := fixture(t)
	prepareReleasedSlot(t, slot, time.Now().Add(-time.Hour))
	if parked, err := retireSlot(slot, time.Now().Add(-time.Second)); parked != "" || err != nil {
		t.Fatalf("expired retirement = %q, %v", parked, err)
	}
	check(t, filepath.Join(slot, "upper"), "payload", "keep")
	files, err := os.ReadDir(filepath.Dir(slot))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), retiredPrefix) {
			t.Fatalf("expired retirement left %s", file.Name())
		}
	}
}

func TestSweepResumesPartiallyDeletedRetirement(t *testing.T) {
	m, _, _, slot := fixture(t)
	store := filepath.Dir(filepath.Dir(slot))
	now := time.Now()
	prepareReleasedSlot(t, slot, now.Add(-2*time.Hour))
	parked, err := retireSlot(slot, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"container.json", releasedMarker} {
		if err := os.Remove(filepath.Join(parked, marker)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 300; i++ {
		put(t, filepath.Join(parked, "upper"), strconv.Itoa(i), "data")
	}
	unknown := filepath.Join(filepath.Dir(slot), retiredPrefix+"unknown")
	mkdir(t, unknown)
	put(t, unknown, "payload", "keep")
	orphan := filepath.Join(filepath.Dir(slot), retiredPrefix+"orphan"+receiptSuffix)
	if err := writeRecord(orphan, retirementReceipt{Inode: 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.Sweep([]string{store}, 0, now); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, orphan)
	assertMissing(t, parked)
	assertMissing(t, parked+".receipt")
	check(t, unknown, "payload", "keep")
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

func TestSweepRejectsWritableStateAncestor(t *testing.T) {
	m, _, _, slot := fixture(t)
	parent := filepath.Join(filepath.Dir(m.Dir), "shared")
	mkdir(t, parent)
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	m.Dir = filepath.Join(parent, "state")
	if err := m.Sweep([]string{filepath.Dir(filepath.Dir(slot))}, 0, time.Now()); err == nil {
		t.Fatal("swept with an untrusted state path")
	}
	assertMissing(t, m.Dir)
}

func TestSweepPreservesWritableStorageSlot(t *testing.T) {
	m, _, _, slot := fixture(t)
	now := time.Now()
	prepareReleasedSlot(t, slot, now.Add(-2*time.Hour))
	if err := os.Chmod(slot, 0777); err != nil {
		t.Fatal(err)
	}
	if err := m.Sweep([]string{filepath.Dir(filepath.Dir(slot))}, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	check(t, filepath.Join(slot, "upper"), "payload", "keep")
}

func TestSweepProtectsSlotByRecordedIdentity(t *testing.T) {
	m, _, root, slot := fixture(t)
	now := time.Now()
	prepareReleasedSlot(t, slot, now.Add(-2*time.Hour))
	info, err := os.Stat(slot)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	mkdir(t, m.Dir)
	state := record{
		Namespace: "mnt:[2]", Root: root, Slot: slot + "-other-path",
		SlotIdentity: &slotIdentity{Device: uint64(stat.Dev), Inode: stat.Ino},
	}
	if err := writeRecord(m.journal("live"), state); err != nil {
		t.Fatal(err)
	}
	if err := m.Sweep([]string{filepath.Dir(filepath.Dir(slot))}, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	check(t, filepath.Join(slot, "upper"), "payload", "keep")
}

func TestPreparePreservesUnsafeUseMarker(t *testing.T) {
	m, _, root, slot := fixture(t)
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "app", UID: "app-uid"}
	if _, err := m.PrepareOwned("first", root, slot, "app", owner); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup("first"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(slot, useMarker)
	if err := os.Chmod(marker, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PrepareOwned("replacement", root, slot, "app", owner); err == nil {
		t.Fatal("accepted a writable use marker")
	}
	if info, err := os.Stat(marker); err != nil || info.Mode().Perm() != 0666 {
		t.Fatalf("use marker changed: %v, %v", info, err)
	}
}

func TestSweepPreservesUnsafeOrphanReceipt(t *testing.T) {
	m, _, _, slot := fixture(t)
	parent := filepath.Dir(slot)
	mkdir(t, parent)
	receipt := filepath.Join(parent, retiredPrefix+"orphan"+receiptSuffix)
	if err := writeRecord(receipt, retirementReceipt{Inode: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(receipt, 0666); err != nil {
		t.Fatal(err)
	}
	if err := m.Sweep([]string{filepath.Dir(parent)}, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(receipt); err != nil {
		t.Fatalf("orphan receipt changed: %v", err)
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
