package oci

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInspectBundleRejectsUnsafeConfig(t *testing.T) {
	bundle := t.TempDir()
	path := filepath.Join(bundle, "config.json")
	target := filepath.Join(bundle, "target")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBundle(bundle, "permabox.io/persist"); err == nil {
		t.Fatal("accepted a symlink")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"+strings.Repeat(" ", 4<<20)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBundle(bundle, "permabox.io/persist"); err == nil {
		t.Fatal("accepted oversized configuration")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBundle(bundle, "permabox.io/persist"); err == nil {
		t.Fatal("accepted a FIFO")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bundle, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBundle(bundle, "permabox.io/persist"); err == nil {
		t.Fatal("accepted a bundle in a writable directory")
	}
}

func TestInvocation(t *testing.T) {
	for _, test := range []struct {
		args []string
		want Invocation
	}{
		{[]string{"--root", "create", "--log=/tmp/log", "create", "--pid-file", "delete", "-b=/bundle", "--", "id"}, Invocation{"create", "id", "/bundle"}},
		{[]string{"--rootless=false", "delete", "--force", "id"}, Invocation{"delete", "id", ""}},
		{[]string{"create", "id"}, Invocation{"create", "id", "."}},
		{[]string{"state", "delete"}, Invocation{"state", "delete", ""}},
		{[]string{"kill", "--all", "real-id", "9"}, Invocation{"kill", "real-id", ""}},
		{[]string{"exec", "--process", "/proc.json", "--detach", "real-id"}, Invocation{"exec", "real-id", ""}},
		{[]string{"update", "--resources=-", "real-id"}, Invocation{"update", "real-id", ""}},
		{[]string{"create", "--bundle"}, Invocation{}},
	} {
		got, err := Parse(test.args)
		if got != test.want || (err != nil) != (test.want == (Invocation{})) {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", test.args, got, err, test.want)
		}
	}
}

func TestInvocationHelpPassesThrough(t *testing.T) {
	for _, args := range [][]string{
		{"create", "--help"},
		{"create", "-h"},
		{"create", "--help", "id"},
	} {
		got, err := Parse(args)
		if err != nil || got != (Invocation{}) {
			t.Errorf("Parse(%q) = %+v, %v; want passthrough", args, got, err)
		}
	}
}

func TestBundleOverrides(t *testing.T) {
	bundle := t.TempDir()
	path := filepath.Join(bundle, "config.json")
	for _, tc := range []struct {
		json, runtime, storage string
	}{
		{`{"annotations":{"permabox.io/underlying-runtime":"sysbox","permabox.io/storage-root":"fast"}}`, "sysbox", ""},
		{`{"annotations":{"io.kubernetes.cri.container-type":"sandbox","permabox.io/underlying-runtime":"sysbox"}}`, "", ""},
		{`{"root":{"path":"rootfs"},"annotations":{"io.kubernetes.cri.sandbox-namespace":"default","io.kubernetes.cri.sandbox-name":"app-0","permabox.io/persist":"true","permabox.io/underlying-runtime":"sysbox","permabox.io/storage-root":"fast"}}`, "sysbox", "fast"},
	} {
		if err := os.WriteFile(path, []byte(tc.json), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := InspectBundle(bundle, "permabox.io/persist")
		if err != nil {
			t.Fatal(err)
		}
		if got.RuntimeAlias != tc.runtime || got.StorageAlias != tc.storage {
			t.Fatalf("choices: %+v", got)
		}
	}
}

func TestBundleSelection(t *testing.T) {
	for _, test := range []struct {
		name, spec        string
		selected, invalid bool
	}{
		{"sandbox", `{"annotations":{"io.kubernetes.cri.container-type":"sandbox","permabox.io/persist":"true"}}`, false, false},
		{"other container", `{"annotations":{"io.kubernetes.cri.container-name":"sidecar","permabox.io/container":"app","permabox.io/persist":"true"}}`, false, false},
		{"case sensitive", `{"annotations":{"permabox.io/persist":"True"}}`, false, false},
		{"selected", `{"root":{"path":"rootfs"},"annotations":{"io.kubernetes.pod.namespace":"default","io.kubernetes.pod.name":"sample-0","permabox.io/persist":"true"}}`, true, false},
		{"containerd spec", `{"root":{"path":"rootfs"},"annotations":{"io.kubernetes.cri.container-type":"container","io.kubernetes.cri.container-name":"app","io.kubernetes.cri.sandbox-namespace":"default","io.kubernetes.cri.sandbox-name":"sample-0","permabox.io/container":"app","permabox.io/persist":"true"}}`, true, false},
		{"traversal", `{"root":{"path":"rootfs"},"annotations":{"io.kubernetes.pod.namespace":"../bad","io.kubernetes.pod.name":"sample-0","permabox.io/persist":"true"}}`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := t.TempDir()
			if err := os.WriteFile(filepath.Join(bundle, "config.json"), []byte(test.spec), 0600); err != nil {
				t.Fatal(err)
			}
			settings, err := InspectBundle(bundle, "permabox.io/persist")
			got := settings.Persistence
			if (err != nil) != test.invalid || (got != nil) != test.selected {
				t.Fatalf("got %+v, %v", got, err)
			}
			if got != nil && got.Root != filepath.Join(bundle, "rootfs") {
				t.Fatalf("unexpected root: %q", got.Root)
			}
		})
	}
}
