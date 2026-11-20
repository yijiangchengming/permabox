//go:build linux

package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yijiangchengming/permabox/internal/mount"
	"github.com/yijiangchengming/permabox/internal/ownership"
)

func TestSweepWorkloadsRechecksEligibilityBeforeRetirement(t *testing.T) {
	base := privateStateDir(t)
	manager := mount.New(base)
	store := filepath.Join(filepath.Dir(base), "store")
	slot := filepath.Join(store, "default", "app-0")
	if err := os.MkdirAll(slot, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	owner := ownership.Owner{Namespace: "default", Kind: "StatefulSet", Name: "app", UID: "app-uid"}
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
	calls := 0
	err := sweepWorkloads(manager, []string{store}, "", 0, now, func(got ownership.Owner) (bool, error) {
		if got != owner {
			t.Fatalf("owner = %+v; want %+v", got, owner)
		}
		calls++
		return calls == 1, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("sweep = %v; policy checks = %d", err, calls)
	}
	if _, err := os.Stat(slot); err != nil {
		t.Fatalf("scale-up should preserve the slot: %v", err)
	}
}
