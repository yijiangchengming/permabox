//go:build linux

package mount

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yijiangchengming/permabox/internal/ownership"
)

func TestRedirectConfigFailureRollsBackMounts(t *testing.T) {
	m, sys, source, slot := fixture(t)
	bundle := filepath.Join(filepath.Dir(source), "bundle")
	mkdir(t, bundle)
	put(t, bundle, "config.json", `{"root":{"path":"/unexpected-root"}}`)
	upstream := entry{ID: 99, Target: source, Kind: "overlay", FSRoot: "/"}
	sys.table = []entry{upstream}

	if _, err := m.PrepareRedirected("id", source, bundle, source, slot, "app", ownership.Owner{}, true, false); err == nil {
		t.Fatal("replaced a bundle pointing to a different root")
	}
	if len(sys.table) != 1 || sys.table[0] != upstream {
		t.Fatalf("rollback changed the upstream mount table: %+v", sys.table)
	}
	assertMissing(t, m.journal("id"))
	assertMissing(t, m.alias("id"))
	assertMissing(t, m.redirectTarget("id"))
	check(t, bundle, "config.json", `{"root":{"path":"/unexpected-root"}}`)

	put(t, bundle, "config.json", `{"root":{"path":"`+source+`"}}`)
	if _, err := m.PrepareRedirected("id", source, bundle, source, slot, "app", ownership.Owner{}, true, false); err != nil {
		t.Fatal(err)
	}
	check(t, bundle, "config.json", `{"root":{"path":"`+m.redirectTarget("id")+`"}}`)
	sys.table[0].ID++
	if _, err := m.PrepareRedirected("id", source, bundle, source, slot, "app", ownership.Owner{}, true, false); err == nil {
		t.Fatal("reused the lower bind after the upstream mount was replaced")
	}
	if err := m.Cleanup("id"); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, m.redirectTarget("id"))
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	check(t, bundle, "config.json", `{"root":{"path":"`+source+`"}}`)
}

func TestRedirectRootWithinContainerdBundle(t *testing.T) {
	m, sys, _, slot := fixture(t)
	sys.table = nil
	bundle := filepath.Join(filepath.Dir(slot), "bundle")
	source := filepath.Join(bundle, "rootfs")
	mkdir(t, source)
	const original = `{"mounts":[{"destination":"/data","source":"relative-source"}],"root":{"path":"rootfs"}}`
	put(t, bundle, "config.json", original)
	if _, err := m.PrepareRedirected("id", source, bundle, "rootfs", slot, "app", ownership.Owner{}, false, false); err != nil {
		t.Fatal(err)
	}
	check(t, bundle, "config.json", `{"mounts":[{"destination":"/data","source":"relative-source"}],"root":{"path":"`+m.redirectTarget("id")+`"}}`)
	if err := m.Cleanup("id"); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, m.redirectTarget("id"))
	check(t, bundle, "config.json", original)
}

func TestRetryLegacyContainerdRoot(t *testing.T) {
	m, sys, source, slot := fixture(t)
	bundle := filepath.Join(filepath.Dir(source), "bundle")
	mkdir(t, bundle)
	newSource := filepath.Join(bundle, "rootfs")
	if err := os.Rename(source, newSource); err != nil {
		t.Fatal(err)
	}
	source = newSource
	sys.table[0].Target = source
	const original = `{"root":{"path":"rootfs"}}`
	put(t, bundle, "config.json", original)
	if _, err := m.Prepare("id", source, slot, "app"); err != nil {
		t.Fatal(err)
	}
	installed, err := m.PrepareRedirected("id", source, bundle, "rootfs", slot, "app", ownership.Owner{}, false, false)
	if err != nil || installed {
		t.Fatalf("retry installed a mount or failed: %v, %v", installed, err)
	}
	check(t, bundle, "config.json", original)
	if err := m.Cleanup("id"); err != nil {
		t.Fatal(err)
	}
}
