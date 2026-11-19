package mount

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	if err := m.ActivateOwnership("replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(second, releasedMarker)); !os.IsNotExist(err) {
		t.Fatalf("claimed release marker remains: %v", err)
	}
}

func TestFailedDeploymentMountPreservesPreviousRelease(t *testing.T) {
	m, sys, root, slot := fixture(t)
	storage := filepath.Dir(filepath.Dir(slot))
	owner := workload.Owner{Namespace: "namespace", Name: "app", Kind: "Deployment", UID: "owner-1"}
	claim := DeploymentClaim{Pod: "app-1", PodUID: "pod-1", Image: "image-a", Runtime: "/usr/bin/runc"}
	prepare := func(id string) error {
		_, err := m.PrepareDeployment(context.Background(), id, root, storage, "demo", owner, claim, 0,
			func(context.Context, string, string) (bool, error) { return true, nil })
		return err
	}
	if err := prepare("original"); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateOwnership("original"); err != nil {
		t.Fatal(err)
	}
	slot, err := m.CleanupSlot("original")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(slot, releasedMarker)
	var before, after time.Time
	if !readMarker(marker, &before) {
		t.Fatal("original release is missing")
	}
	claim.Pod, claim.PodUID = "app-2", "pod-2"
	sys.fail = true
	if err := prepare("failed"); err == nil {
		t.Fatal("expected overlay mount failure")
	}
	if !readMarker(marker, &after) || !before.Equal(after) {
		t.Fatal("failed claim lost the previous release record")
	}
	sys.fail = false
	if err := prepare("retry"); err != nil {
		t.Fatal(err)
	}
	got, err := readRecord(m.journal("retry"))
	if err != nil || got.Slot != slot {
		t.Fatalf("previous data is unavailable to a later claim: %+v, %v", got, err)
	}
}
