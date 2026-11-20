//go:build linux

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/yijiangchengming/permabox/internal/mount"
	"github.com/yijiangchengming/permabox/internal/ownership"
)

type reclaimDecision struct {
	reason     mount.ReclaimReason
	generation int64
}

type reclaimer func(context.Context, ownership.Owner) (reclaimDecision, error)

type gcLimits struct {
	owners                                   int
	ownerTimeout, firstBudget, confirmBudget time.Duration
}

const gcCursor = ".permabox-gc-cursor"

func sweepWorkloads(manager mount.Manager, roots []string, target string, ttl time.Duration, now time.Time, reclaim reclaimer) error {
	limits := gcLimits{owners: 3, ownerTimeout: 2 * time.Second, firstBudget: 7 * time.Second, confirmBudget: 7 * time.Second}
	return sweepWorkloadBatch(manager, roots, target, ttl, now, limits, reclaim)
}

func sweepWorkloadBatch(manager mount.Manager, roots []string, target string, ttl time.Duration, now time.Time, limits gcLimits, reclaim reclaimer) error {
	candidates, sweepErr := manager.DiscoverReleased(roots, target, ttl, now)
	if len(candidates) == 0 {
		return sweepErr
	}
	deleted, scaledDown, checkErr := checkWorkloadBatch(manager.Dir, candidates, limits, reclaim)
	if len(deleted) > 0 {
		sweepErr = errors.Join(sweepErr, manager.RetireReleased(roots, target, now, deleted))
	}
	for _, a := range scaledDown {
		sweepErr = errors.Join(sweepErr, manager.StageScaledDown(roots, target, now, a.slots, a.generation))
	}
	return errors.Join(sweepErr, checkErr)
}

type ownerGroup struct {
	owner ownership.Owner
	slots []mount.ReleasedSlot
}

type scaleDownApproval struct {
	slots      []mount.ReleasedSlot
	generation int64
}

func checkWorkloadBatch(dir string, candidates []mount.ReleasedSlot, limits gcLimits, reclaim reclaimer) (deleted []mount.ReleasedSlot, scaledDown []scaleDownApproval, result error) {
	// Only policy checks take this lock. Mount operations use a different lock.
	lock, err := os.OpenFile(filepath.Join(dir, gcCursor+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("open GC cursor lock: %w", err)
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, nil, nil // A concurrent sweep is already advancing this cursor.
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lock GC cursor: %w", err)
	}
	cursorPath := filepath.Join(dir, gcCursor)
	cursor, err := os.ReadFile(cursorPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("read GC cursor: %w", err)
	}
	groups := make(map[string]*ownerGroup)
	var keys []string
	for _, slot := range candidates {
		owner := slot.Owner()
		encoded, err := json.Marshal(owner)
		if err != nil {
			return nil, nil, err
		}
		key := string(encoded) // Includes policy overrides; those can differ for the same workload.
		group := groups[key]
		if group == nil {
			group = &ownerGroup{owner: owner}
			groups[key] = group
			keys = append(keys, key)
		}
		group.slots = append(group.slots, slot)
	}
	sort.Strings(keys)
	start := sort.Search(len(keys), func(i int) bool { return keys[i] > string(cursor) })
	last := ""
	type approval struct {
		group    *ownerGroup
		decision reclaimDecision
	}
	var approved []approval
	first, cancelFirst := context.WithTimeout(context.Background(), limits.firstBudget)
	for i := 0; i < min(limits.owners, len(keys)) && first.Err() == nil; i++ {
		last = keys[(start+i)%len(keys)]
		group := groups[last]
		decision, err := checkReclaim(first, limits.ownerTimeout, group.owner, reclaim)
		result = errors.Join(result, err)
		if decision.reason == mount.ReclaimDeleted || (decision.reason == mount.ReclaimZero && decision.generation > 0) {
			approved = append(approved, approval{group: group, decision: decision})
		}
	}
	cancelFirst()
	if last != "" {
		result = errors.Join(result, writeGCCursor(cursorPath, last))
	}
	confirm, cancelConfirm := context.WithTimeout(context.Background(), limits.confirmBudget)
	defer cancelConfirm()
	for _, a := range approved {
		if confirm.Err() != nil {
			break
		}
		decision, err := checkReclaim(confirm, limits.ownerTimeout, a.group.owner, reclaim)
		result = errors.Join(result, err)
		if decision != a.decision {
			continue
		}
		switch decision.reason {
		case mount.ReclaimDeleted:
			deleted = append(deleted, a.group.slots...)
		case mount.ReclaimZero:
			scaledDown = append(scaledDown, scaleDownApproval{slots: a.group.slots, generation: decision.generation})
		}
	}
	return deleted, scaledDown, result
}

func checkReclaim(parent context.Context, timeout time.Duration, owner ownership.Owner, reclaim reclaimer) (reclaimDecision, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	decision, err := reclaim(ctx, owner)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return reclaimDecision{}, fmt.Errorf("check workload %s/%s (%s): %w", owner.Namespace, owner.Name, owner.UID, err)
	}
	return decision, nil
}

func writeGCCursor(path, key string) error {
	f, err := os.CreateTemp(filepath.Dir(path), gcCursor+"-")
	if err != nil {
		return fmt.Errorf("create GC cursor: %w", err)
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(key)
	if err := errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("write GC cursor: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace GC cursor: %w", err)
	}
	return nil
}
