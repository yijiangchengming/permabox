//go:build linux

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/yijiangchengming/permabox/internal/mount"
	"github.com/yijiangchengming/permabox/internal/ownership"
	"github.com/yijiangchengming/permabox/internal/trustedpath"
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
const maxGCCursorSize = 4096

func sweepWorkloads(manager mount.Manager, roots []string, target string, ttl time.Duration, now time.Time, reclaim reclaimer) error {
	limits := gcLimits{owners: 3, ownerTimeout: 2 * time.Second, firstBudget: 7 * time.Second, confirmBudget: 7 * time.Second}
	return sweepWorkloadBatch(manager, roots, target, ttl, now, limits, reclaim)
}

func sweepWorkloadBatch(manager mount.Manager, roots []string, target string, ttl time.Duration, now time.Time, limits gcLimits, reclaim reclaimer) error {
	startedAt := time.Now()
	if now.IsZero() {
		now = startedAt
	}
	candidates, sweepErr := manager.DiscoverReleased(roots, target, ttl, now)
	if len(candidates) == 0 {
		return sweepErr
	}
	checkErr := checkWorkloadBatch(manager.Dir, candidates, limits, reclaim, func(slots []mount.ReleasedSlot, decision reclaimDecision, checkedAt time.Time) error {
		switch decision.reason {
		case mount.ReclaimDeleted:
			return manager.RetireReleased(roots, target, now, slots)
		case mount.ReclaimZero:
			approvedAt := now.Add(checkedAt.Sub(startedAt))
			return manager.StageScaledDownBefore(roots, target, approvedAt, slots, decision.generation, checkedAt.Add(2*time.Second))
		default:
			return nil
		}
	})
	return errors.Join(sweepErr, checkErr)
}

type ownerGroup struct {
	owner ownership.Owner
	slots []mount.ReleasedSlot
}

func checkWorkloadBatch(dir string, candidates []mount.ReleasedSlot, limits gcLimits, reclaim reclaimer,
	apply func([]mount.ReleasedSlot, reclaimDecision, time.Time) error) (result error) {
	state, err := trustedpath.OpenDir(dir, false, true)
	if err != nil {
		return fmt.Errorf("open GC state directory: %w", err)
	}
	defer state.Close()
	// Only policy checks take this lock. Mount operations use a different lock.
	lock, err := trustedpath.OpenRegularAt(state, gcCursor+".lock", syscall.O_CREAT|syscall.O_RDWR)
	if err != nil {
		return fmt.Errorf("open GC cursor lock: %w", err)
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil // A concurrent sweep is already advancing this cursor.
	}
	if err != nil {
		return fmt.Errorf("lock GC cursor: %w", err)
	}
	cursorPath := filepath.Join(trustedpath.FDPath(state), gcCursor)
	cursor, err := readGCCursor(state)
	if err != nil {
		return fmt.Errorf("read GC cursor: %w", err)
	}
	groups := make(map[string]*ownerGroup)
	var keys []string
	for _, slot := range candidates {
		owner := slot.Owner()
		encoded, err := json.Marshal(owner)
		if err != nil {
			return err
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
	remaining := limits.confirmBudget
	for _, a := range approved {
		if remaining <= 0 {
			break
		}
		confirm, cancelConfirm := context.WithTimeout(context.Background(), remaining)
		started := time.Now()
		decision, err := checkReclaim(confirm, limits.ownerTimeout, a.group.owner, reclaim)
		checkedAt := time.Now()
		cancelConfirm()
		remaining -= checkedAt.Sub(started)
		result = errors.Join(result, err)
		if err == nil && decision == a.decision {
			result = errors.Join(result, apply(a.group.slots, decision, checkedAt))
		}
	}
	return result
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

func readGCCursor(state *os.File) ([]byte, error) {
	file, err := trustedpath.OpenRegularAt(state, gcCursor, syscall.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxGCCursorSize+1))
	if len(data) > maxGCCursorSize {
		return nil, errors.New("GC cursor exceeds 4096 bytes")
	}
	return data, err
}

func writeGCCursor(path, key string) error {
	if len(key) > maxGCCursorSize {
		return errors.New("GC cursor exceeds 4096 bytes")
	}
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
