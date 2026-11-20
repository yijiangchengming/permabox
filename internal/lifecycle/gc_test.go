//go:build linux

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yijiangchengming/permabox/internal/mount"
	"github.com/yijiangchengming/permabox/internal/ownership"
)

func TestSweepWorkloadsRechecksEligibilityBeforeRetirement(t *testing.T) {
	base := privateStateDir(t)
	manager := mount.New(base)
	store := filepath.Join(filepath.Dir(base), "store")
	now := time.Now()
	owner := gcTestOwner("app")
	slot := writeGCSlot(t, store, "app-0", owner, now)
	calls := 0
	err := sweepWorkloads(manager, []string{store}, "", 0, now, func(_ context.Context, got ownership.Owner) (reclaimDecision, error) {
		if got != owner {
			t.Fatalf("owner = %+v; want %+v", got, owner)
		}
		calls++
		if calls == 1 {
			return reclaimDecision{reason: mount.ReclaimZero, generation: 1}, nil
		}
		return reclaimDecision{}, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("sweep = %v; policy checks = %d", err, calls)
	}
	if _, err := os.Stat(slot); err != nil {
		t.Fatalf("scale-up should preserve the slot: %v", err)
	}
}

func TestSweepWorkloadsRequiresMatchingReason(t *testing.T) {
	base := privateStateDir(t)
	store := filepath.Join(filepath.Dir(base), "store")
	now := time.Now()
	slot := writeGCSlot(t, store, "app-0", gcTestOwner("app"), now)
	calls := 0
	err := sweepWorkloads(mount.New(base), []string{store}, "", 0, now, func(_ context.Context, _ ownership.Owner) (reclaimDecision, error) {
		calls++
		if calls == 1 {
			return reclaimDecision{reason: mount.ReclaimZero, generation: 1}, nil
		}
		return reclaimDecision{reason: mount.ReclaimDeleted}, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("sweep = %v; policy checks = %d", err, calls)
	}
	if _, err := os.Stat(slot); err != nil {
		t.Fatalf("changed reason should preserve the slot: %v", err)
	}
}

func TestSweepWorkloadsRoutesConfirmedReasons(t *testing.T) {
	base := privateStateDir(t)
	store := filepath.Join(filepath.Dir(base), "store")
	roots := []string{store}
	now := time.Now()
	deleted := writeGCSlot(t, store, "deleted-0", gcTestOwner("deleted"), now)
	zero := writeGCSlot(t, store, "zero-0", gcTestOwner("zero"), now)
	reclaim := func(_ context.Context, owner ownership.Owner) (reclaimDecision, error) {
		if owner.Name == "deleted" {
			return reclaimDecision{reason: mount.ReclaimDeleted}, nil
		}
		return reclaimDecision{reason: mount.ReclaimZero, generation: 1}, nil
	}
	manager := mount.New(base)
	if err := sweepWorkloads(manager, roots, "", 0, now, reclaim); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(deleted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed deletion must retire immediately: %v", err)
	}
	if _, err := os.Stat(zero); err != nil {
		t.Fatalf("initial scale-to-zero approval must preserve the slot: %v", err)
	}
	if err := sweepWorkloads(manager, roots, "", 0, now.Add(time.Hour), reclaim); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(zero); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed scale-to-zero after grace must retire the slot: %v", err)
	}
}

func TestSweepWorkloadsConfirmsEachGroupAfterPreviousRetirement(t *testing.T) {
	base := privateStateDir(t)
	store := filepath.Join(filepath.Dir(base), "store")
	roots := []string{store}
	now := time.Now()
	manager := mount.New(base)
	slot := writeGCSlot(t, store, "b-0", gcTestOwner("b"), now)
	zero := reclaimDecision{reason: mount.ReclaimZero, generation: 1}
	if err := sweepWorkloads(manager, roots, "", 0, now, func(context.Context, ownership.Owner) (reclaimDecision, error) {
		return zero, nil
	}); err != nil {
		t.Fatal(err)
	}
	previous := writeGCSlot(t, store, "a-0", gcTestOwner("a"), now)
	checks := 0
	err := sweepWorkloads(manager, roots, "", 0, now.Add(time.Hour), func(_ context.Context, owner ownership.Owner) (reclaimDecision, error) {
		if owner.Name == "a" {
			return reclaimDecision{reason: mount.ReclaimDeleted}, nil
		}
		checks++
		if checks == 2 {
			if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("previous group should be retired before the last check: %v", err)
			}
			return reclaimDecision{}, nil // Desired replicas changed during earlier cleanup.
		}
		return zero, nil
	})
	if _, statErr := os.Stat(slot); err != nil || checks != 2 || statErr != nil {
		t.Fatalf("sweep=%v checks=%d slot=%v", err, checks, statErr)
	}
}

func TestSweepWorkloadsRestartsGraceAfterGenerationChange(t *testing.T) {
	base := privateStateDir(t)
	store := filepath.Join(filepath.Dir(base), "store")
	roots := []string{store}
	now := time.Now()
	slot := writeGCSlot(t, store, "app-0", gcTestOwner("app"), now)
	manager := mount.New(base)
	sweep := func(at time.Time, generations ...int64) {
		t.Helper()
		i := 0
		err := sweepWorkloads(manager, roots, "", 0, at, func(_ context.Context, _ ownership.Owner) (reclaimDecision, error) {
			i++
			return reclaimDecision{reason: mount.ReclaimZero, generation: generations[i-1]}, nil
		})
		if err != nil || i != len(generations) {
			t.Fatalf("sweep: %v; checks = %d, want %d", err, i, len(generations))
		}
	}
	sweep(now, 1, 1)
	sweep(now.Add(time.Hour), 1, 2)
	if _, err := os.Stat(slot); err != nil {
		t.Fatalf("changed generation during confirmation must preserve slot: %v", err)
	}
	sweep(now.Add(2*time.Hour), 2, 2)
	if _, err := os.Stat(slot); err != nil {
		t.Fatalf("newly confirmed generation must restart grace: %v", err)
	}
	sweep(now.Add(3*time.Hour), 2, 2)
	if _, err := os.Stat(slot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("same generation after grace must retire slot: %v", err)
	}
}

func gcTestOwner(name string) ownership.Owner {
	return ownership.Owner{Namespace: "default", Kind: "StatefulSet", Name: name, UID: name + "-uid"}
}

func writeGCSlot(t *testing.T, store, name string, owner ownership.Owner, now time.Time) string {
	t.Helper()
	slot := filepath.Join(store, "default", name)
	if err := os.MkdirAll(slot, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{
		"container.json": "app", ".permabox-released.json": now, ".permabox-workload.json": owner,
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(slot, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return slot
}

func TestSweepWorkloadsRotatesBoundedOwnerBatches(t *testing.T) {
	base := privateStateDir(t)
	store := filepath.Join(filepath.Dir(base), "store")
	now := time.Now()
	for i, name := range []string{"a", "a", "b", "c", "d", "e"} {
		writeGCSlot(t, store, string(rune('a'+i)), gcTestOwner(name), now)
	}
	limits := gcLimits{owners: 2, ownerTimeout: time.Second, firstBudget: 3 * time.Second, confirmBudget: 3 * time.Second}
	for _, want := range [][]string{{"a", "b"}, {"c", "d"}, {"e", "a"}} {
		var got []string
		err := sweepWorkloadBatch(mount.New(base), []string{store}, "", 0, now, limits, func(_ context.Context, owner ownership.Owner) (reclaimDecision, error) {
			got = append(got, owner.Name)
			return reclaimDecision{}, nil
		})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("sweep owners = %v, %v; want %v", got, err, want)
		}
	}
}

func TestSweepWorkloadsConfirmationHasIndependentBudget(t *testing.T) {
	base := privateStateDir(t)
	store := filepath.Join(filepath.Dir(base), "store")
	now := time.Now()
	for _, name := range []string{"a", "b", "c"} {
		writeGCSlot(t, store, name, gcTestOwner(name), now)
	}
	limits := gcLimits{owners: 3, ownerTimeout: 2 * time.Second, firstBudget: 100 * time.Millisecond, confirmBudget: time.Second}
	var got []string
	err := sweepWorkloadBatch(mount.New(base), []string{store}, "", 0, now, limits, func(ctx context.Context, owner ownership.Owner) (reclaimDecision, error) {
		got = append(got, owner.Name)
		if owner.Name == "b" {
			<-ctx.Done()
			return reclaimDecision{reason: mount.ReclaimDeleted}, ctx.Err() // A positive result must fail closed on timeout.
		}
		if ctx.Err() != nil {
			t.Fatalf("%s got an expired context: %v", owner.Name, ctx.Err())
		}
		if len(got) == 1 {
			return reclaimDecision{reason: mount.ReclaimDeleted}, nil
		}
		return reclaimDecision{}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(got, []string{"a", "b", "a"}) {
		t.Fatalf("sweep owners = %v, %v; want initial a, b and confirmed a", got, err)
	}
	got = nil
	limits.owners = 1
	err = sweepWorkloadBatch(mount.New(base), []string{store}, "", 0, now, limits, func(_ context.Context, owner ownership.Owner) (reclaimDecision, error) {
		got = append(got, owner.Name)
		return reclaimDecision{}, nil
	})
	if err != nil || !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("next sweep owners = %v, %v; want c", got, err)
	}
}

func TestSweepWorkloadsLimitsEachOwnerLookup(t *testing.T) {
	base := privateStateDir(t)
	store := filepath.Join(filepath.Dir(base), "store")
	now := time.Now()
	for _, name := range []string{"a", "b"} {
		writeGCSlot(t, store, name, gcTestOwner(name), now)
	}
	limits := gcLimits{owners: 2, ownerTimeout: 20 * time.Millisecond, firstBudget: time.Second, confirmBudget: time.Second}
	var got []string
	err := sweepWorkloadBatch(mount.New(base), []string{store}, "", 0, now, limits, func(ctx context.Context, owner ownership.Owner) (reclaimDecision, error) {
		got = append(got, owner.Name)
		if owner.Name == "a" {
			<-ctx.Done()
			return reclaimDecision{reason: mount.ReclaimDeleted}, nil // Expired even if the callback forgot to report it.
		}
		if len(got) == 2 && ctx.Err() == nil {
			return reclaimDecision{reason: mount.ReclaimDeleted}, nil
		}
		return reclaimDecision{}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(got, []string{"a", "b", "b"}) {
		t.Fatalf("sweep owners = %v, %v; want a, b, confirmed b", got, err)
	}
}
