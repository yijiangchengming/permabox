//go:build linux

package lifecycle

import (
	"errors"
	"time"

	"github.com/yijiangchengming/permabox/internal/mount"
	"github.com/yijiangchengming/permabox/internal/ownership"
)

func sweepWorkloads(manager mount.Manager, roots []string, target string, ttl time.Duration, now time.Time, reclaim func(ownership.Owner) (bool, error)) error {
	candidates, sweepErr := manager.DiscoverReleased(roots, target, ttl, now)
	approved, checkErr := approvedReleases(candidates, reclaim)
	// Recheck without holding the mount lock: a scale-up during discovery can cancel GC.
	approved, confirmErr := approvedReleases(approved, reclaim)
	return errors.Join(sweepErr, checkErr, confirmErr, manager.RetireReleased(roots, target, now, approved))
}

func approvedReleases(candidates []mount.ReleasedSlot, reclaim func(ownership.Owner) (bool, error)) ([]mount.ReleasedSlot, error) {
	var approved []mount.ReleasedSlot
	var result error
	checked := make(map[ownership.Owner]bool)
	for _, slot := range candidates {
		owner := slot.Owner()
		ok, exists := checked[owner]
		if !exists {
			var err error
			ok, err = reclaim(owner)
			if err != nil {
				result = errors.Join(result, err)
				ok = false
			}
			checked[owner] = ok
		}
		if ok {
			approved = append(approved, slot)
		}
	}
	return approved, result
}
