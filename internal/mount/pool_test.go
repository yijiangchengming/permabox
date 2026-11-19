package mount

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yijiangchengming/permabox/internal/workload"
)

func TestDeploymentSlotCannotBeSharedOrStolenFromLivePod(t *testing.T) {
	m, _, root, slot := fixture(t)
	storage := filepath.Dir(filepath.Dir(slot))
	owner := workload.Owner{Namespace: "namespace", Name: "app", Kind: "Deployment", UID: "owner-1"}
	claim := DeploymentClaim{Pod: "app-1", PodUID: "pod-1", Image: "image-a", Runtime: "/usr/bin/runc"}
	prepare := func(id string, c DeploymentClaim, finished bool) (bool, error) {
		return m.PrepareDeployment(context.Background(), id, root, storage, "demo", owner, c, 0,
			func(context.Context, string, string) (bool, error) { return finished, nil })
	}
	if installed, err := prepare("first", claim, false); err != nil || !installed {
		t.Fatalf("first prepare: %v, %v", installed, err)
	}
	if err := m.ActivateOwnership("first"); err != nil {
		t.Fatal(err)
	}
	first, err := m.CleanupSlot("first")
	if err != nil {
		t.Fatal(err)
	}
	other := claim
	other.Pod, other.PodUID = "app-2", "pod-2"
	if _, err := prepare("other", other, false); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateOwnership("other"); err != nil {
		t.Fatal(err)
	}
	second, err := m.CleanupSlot("other")
	if err != nil || first == second {
		t.Fatalf("live Pod layer was reused: %q, %v", second, err)
	}
	if _, err := prepare("restart", claim, false); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateOwnership("restart"); err != nil {
		t.Fatal(err)
	}
	restart, err := m.CleanupSlot("restart")
	if err != nil || restart != first {
		t.Fatalf("same Pod did not get its layer: %q, %v", restart, err)
	}
	if _, err := prepare("replacement", other, true); err != nil {
		t.Fatal(err)
	}
	replacement, err := readRecord(m.journal("replacement"))
	if err != nil || replacement.Slot != second {
		t.Fatalf("same Pod should win over eligible peers: %+v, %v", replacement, err)
	}
	if _, err := os.Stat(filepath.Join(second, releasedMarker)); !os.IsNotExist(err) {
		t.Fatalf("claimed release marker remains: %v", err)
	}
}
