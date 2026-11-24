//go:build linux

package mount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/yijiangchengming/permabox/internal/ownership"
)

func TestResolveExclusionDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cache", "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("cache", filepath.Join(root, "var-cache")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cache", "file"), []byte("image"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		paths []string
		want  []string
		valid bool
	}{
		{name: "empty", valid: true},
		{name: "directory", paths: []string{"/cache"}, want: []string{"cache"}, valid: true},
		{name: "symlink", paths: []string{"/var-cache"}, want: []string{"cache"}, valid: true},
		{name: "missing", paths: []string{"/missing"}},
		{name: "file", paths: []string{"/cache/file"}},
		{name: "overlapping", paths: []string{"/cache", "/cache/nested"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveExclusionDirectories(root, test.paths)
			if (err == nil) != test.valid {
				t.Fatalf("resolveExclusionDirectories error = %v", err)
			}
			if test.valid && strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("resolved paths = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPrepareMountsAndCleansTemporaryExclusionOverlay(t *testing.T) {
	m, sys, root, slot := fixture(t)
	if err := os.Mkdir(filepath.Join(root, "cache"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "cache"), 0755); err != nil {
		t.Fatal(err)
	}
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "pod", UID: "uid", Excludes: []string{"/cache"}}
	if _, err := m.PrepareOwned("id", root, slot, "app", owner); err != nil {
		t.Fatal(err)
	}
	state, err := readRecord(m.journal("id"))
	if err != nil || len(state.ExclusionMounts) != 1 {
		t.Fatalf("exclusion journal = %+v, %v", state.ExclusionMounts, err)
	}
	exclusion := state.ExclusionMounts[0]
	if exclusion.Path != "cache" || exclusion.MountID == 0 {
		t.Fatalf("exclusion mount record = %+v", exclusion)
	}
	upper, err := os.Stat(filepath.Join(exclusion.Scratch, "upper"))
	if err != nil || upper.Mode().Perm() != 0755 {
		t.Fatalf("unexpected exclusion upper permissions: %v, %v", upper, err)
	}
	target := filepath.Join(root, "cache")
	if !hasMountTarget(sys.table, target, exclusion.MountID) {
		t.Fatalf("temporary overlay not mounted at %s: %+v", target, sys.table)
	}
	if err := m.Cleanup("id"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exclusion.Scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch directory remains after cleanup: %v", err)
	}
	if len(sys.table) != 1 {
		t.Fatalf("mounts remain after cleanup: %+v", sys.table)
	}
}

func TestFailedExclusionMountRollsBackAllMounts(t *testing.T) {
	m, sys, root, slot := fixture(t)
	if err := os.Mkdir(filepath.Join(root, "cache"), 0755); err != nil {
		t.Fatal(err)
	}
	sys.failAfterRootOverlay = true
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "pod", UID: "uid", Excludes: []string{"/cache"}}
	if _, err := m.PrepareOwned("id", root, slot, "app", owner); err == nil {
		t.Fatal("accepted failed temporary overlay mount")
	}
	if len(sys.table) != 1 {
		t.Fatalf("mount rollback left mounts behind: %+v", sys.table)
	}
	if _, err := os.Stat(m.journal("id")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains after rollback: %v", err)
	}
}

func TestContainerRetryRequiresSameExclusionDirectories(t *testing.T) {
	m, _, root, slot := fixture(t)
	for _, path := range []string{"cache", "other"} {
		if err := os.Mkdir(filepath.Join(root, path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "pod", UID: "uid", Excludes: []string{"/cache"}}
	if _, err := m.PrepareOwned("id", root, slot, "app", owner); err != nil {
		t.Fatal(err)
	}
	owner.Excludes = []string{"/other"}
	if _, err := m.PrepareOwned("id", root, slot, "app", owner); err == nil {
		t.Fatal("accepted retry with changed exclusion directories")
	}
	if err := m.Cleanup("id"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRejectsReplacedExclusionMount(t *testing.T) {
	m, sys, root, slot := fixture(t)
	if err := os.Mkdir(filepath.Join(root, "cache"), 0755); err != nil {
		t.Fatal(err)
	}
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "pod", UID: "uid", Excludes: []string{"/cache"}}
	if _, err := m.PrepareOwned("id", root, slot, "app", owner); err != nil {
		t.Fatal(err)
	}
	for i := range sys.table {
		if sys.table[i].Target == filepath.Join(root, "cache") {
			sys.table[i].ID++
		}
	}
	if err := m.Cleanup("id"); err == nil {
		t.Fatal("cleaned up after exclusion mount identity changed")
	}
	if _, err := os.Stat(m.journal("id")); err != nil {
		t.Fatalf("journal not retained after identity mismatch: %v", err)
	}
}

func TestPrepareRejectsMissingAndOverlappingExclusionDirectoriesBeforeMount(t *testing.T) {
	for _, paths := range [][]string{{"/missing"}, {"/cache", "/cache/nested"}} {
		t.Run(strings.Join(paths, ","), func(t *testing.T) {
			m, sys, root, slot := fixture(t)
			if err := os.Mkdir(filepath.Join(root, "cache"), 0755); err != nil {
				t.Fatal(err)
			}
			owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "pod", UID: "uid", Excludes: paths}
			if _, err := m.PrepareOwned("id", root, slot, "app", owner); err == nil {
				t.Fatal("accepted invalid exclusion directory list")
			}
			if len(sys.table) != 1 {
				t.Fatalf("mounted before rejecting exclusions: %+v", sys.table)
			}
		})
	}
}

func TestReadRecordRejectsEscapingExclusionPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	state := record{
		Namespace: "mnt:[1]",
		Root:      filepath.Join(dir, "root"),
		Slot:      filepath.Join(dir, "slot"),
		ExclusionMounts: []exclusionMount{{
			Path:    "../../outside",
			Scratch: filepath.Join(dir, "scratch"),
		}},
	}
	if err := writeRecord(path, state); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecord(path); err == nil {
		t.Fatal("accepted an exclusion path that escapes the container root")
	}
}

func TestExclusionScratchDirsAreRemovedOnlyAfterUnmount(t *testing.T) {
	m, sys, root, slot := fixture(t)
	if err := os.Mkdir(filepath.Join(root, "cache"), 0755); err != nil {
		t.Fatal(err)
	}
	owner := ownership.Owner{Namespace: "namespace", Kind: "StatefulSet", Name: "pod", UID: "uid", Excludes: []string{"/cache"}}
	if _, err := m.PrepareOwned("id", root, slot, "app", owner); err != nil {
		t.Fatal(err)
	}
	state, err := readRecord(m.journal("id"))
	if err != nil {
		t.Fatal(err)
	}
	sys.busyTarget = filepath.Join(root, "cache")
	if err := m.Cleanup("id"); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("cleanup error = %v, want EBUSY", err)
	}
	if _, err := os.Stat(state.ExclusionMounts[0].Scratch); err != nil {
		t.Fatalf("scratch removed while mount remains: %v", err)
	}
}

func TestCleanupLegacyExclusionScratch(t *testing.T) {
	m, sys, root, slot := fixture(t)
	mkdir(t, filepath.Join(root, "cache"))
	if _, err := m.Prepare("id", root, slot, "app"); err != nil {
		t.Fatal(err)
	}
	state, err := readRecord(m.journal("id"))
	if err != nil {
		t.Fatal(err)
	}
	item := exclusionMount{Path: "cache", Scratch: filepath.Join(m.Dir, "id.exclude-"+exclusionHash("cache"))}
	mkdir(t, filepath.Join(item.Scratch, "upper"))
	target := filepath.Join(root, item.Path)
	if err := sys.mount("overlay", target, "overlay", 0, exclusionOverlayOptions(m.alias("id"), item)); err != nil {
		t.Fatal(err)
	}
	item.MountID = sys.table[len(sys.table)-1].ID
	state.ExclusionMounts = []exclusionMount{item}
	if err := writeRecord(m.journal("id"), state); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup("id"); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, item.Scratch)
	assertMissing(t, m.journal("id"))
}

func hasMountTarget(mounts []entry, target string, id int) bool {
	for _, mount := range mounts {
		if mount.Target == target && mount.ID == id && mount.Kind == "overlay" {
			return true
		}
	}
	return false
}

func TestFindExclusionOverlayRejectsMovedMount(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "rootfs")
	item := exclusionMount{Path: "cache", Scratch: filepath.Join(dir, "scratch"), MountID: 43}
	table := []entry{{ID: item.MountID, Target: filepath.Join(root, "other"), Kind: "overlay"}}
	if _, err := findExclusionOverlay(table, record{Root: root}, filepath.Join(dir, "lower"), item); err == nil {
		t.Fatal("accepted moved exclusion mount")
	}
}
