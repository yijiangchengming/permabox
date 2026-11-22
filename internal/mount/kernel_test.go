//go:build linux

package mount

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/yijiangchengming/permabox/internal/ownership"
)

// Run with: unshare --mount --propagation private env PERMABOX_KERNEL_TEST=1 go test -v ./internal/mount -run TestKernelPersistence
func TestKernelPersistence(t *testing.T) {
	if os.Getenv("PERMABOX_KERNEL_TEST") != "1" {
		t.Skip("requires CAP_SYS_ADMIN in a private mount namespace")
	}
	base, err := os.MkdirTemp("/dev/shm", "permabox-test ")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	m := New(filepath.Join(base, "state"))
	image := filepath.Join(base, "image")
	mkdir(t, image)
	put(t, image, "edited", "from image")
	put(t, image, "deleted", "image file")
	slot := filepath.Join(base, "storage", "default", "app-0")
	roots := []string{filepath.Dir(filepath.Dir(slot))}
	owner := ownership.Owner{Namespace: "default", Name: "app", Kind: "StatefulSet", UID: "workload-uid"}
	for _, id := range []string{"first", "replacement"} {
		func() {
			bundle := filepath.Join(base, id)
			root := filepath.Join(bundle, "rootfs")
			upper := filepath.Join(bundle, "snapshot-upper")
			work := filepath.Join(bundle, "snapshot-work")
			for _, dir := range []string{root, upper, work} {
				mkdir(t, dir)
			}
			if err := os.Chmod(upper, 0751); err != nil {
				t.Fatal(err)
			}
			opts := "lowerdir=" + image + ",upperdir=" + upper + ",workdir=" + work
			if err := syscall.Mount("overlay", root, "overlay", 0, opts); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Unmount(root, syscall.MNT_DETACH) })
			if err := syscall.Mount("", root, "", syscall.MS_SHARED, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := m.PrepareOwned(id, root, slot, "app", owner); err != nil {
				t.Fatal(err)
			}
			mounts, err := m.sys.mounts()
			if err != nil {
				t.Fatal(err)
			}
			if lower, err := lowerMountID(mounts, m.alias(id)); lower == 0 || err != nil {
				t.Fatalf("expected one private lower mount, got %d, %v", lower, err)
			}
			if err := m.ActivateOwnership(id); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Cleanup(id) })
			if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0751 {
				t.Fatalf("rootfs permissions changed after installing upper: %v, %v", info, err)
			}
			if id == "first" {
				put(t, root, "edited", "persisted edit")
				put(t, root, "created", "persisted addition")
				if err := os.Remove(filepath.Join(root, "deleted")); err != nil {
					t.Fatal(err)
				}
			} else {
				check(t, root, "edited", "persisted edit")
				check(t, root, "created", "persisted addition")
				if _, err := os.Stat(filepath.Join(root, "deleted")); !os.IsNotExist(err) {
					t.Fatalf("whiteout did not survive: %v", err)
				}
			}
			released, err := m.CleanupSlot(id)
			if err != nil || released != slot {
				t.Fatalf("cleanup = %q, %v", released, err)
			}
			now := time.Now()
			candidates, err := m.DiscoverReleased(roots, released, 0, now)
			if err != nil || len(candidates) != 1 || candidates[0].Owner() != owner {
				t.Fatalf("discover = %+v, %v", candidates, err)
			}
			// The first release is kept so its replacement can reuse the layer.
			if id == "replacement" {
				if err := m.RetireReleased(roots, released, now, candidates); err != nil {
					t.Fatal(err)
				}
			}
			check(t, root, "edited", "from image")
			check(t, root, "deleted", "image file")
			if _, err := os.Stat(m.alias(id)); !os.IsNotExist(err) {
				t.Fatalf("alias was not removed: %v", err)
			}
			if err := syscall.Unmount(root, 0); err != nil {
				t.Fatal(err)
			}
		}()
	}
	if _, err := os.Stat(slot); !os.IsNotExist(err) {
		t.Fatalf("released slot still exists after immediate reclaim: %v", err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, dir, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || string(got) != want {
		t.Fatalf("%s: got %q, %v; want %q", name, got, err, want)
	}
}
