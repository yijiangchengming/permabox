package oci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCRIOInvocation(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, tc := range []struct {
		args string
		want Invocation
	}{
		{"--systemd-cgroup --root=/run/runc create --bundle /bundle --pid-file /bundle/pidfile --no-pivot --no-new-keyring --console-socket /run/conmon.sock", Invocation{"create", id, "/bundle"}},
		{"--root /run/runc --systemd-cgroup start", Invocation{"start", id, ""}},
		{"--root /run/runc --systemd-cgroup delete --force", Invocation{"delete", id, ""}},
		{"--root /run/runc --systemd-cgroup state", Invocation{"state", id, ""}},
		{"--root=/run/runc exec --pid-file /pid --process /process.json --detach", Invocation{"exec", id, ""}},
	} {
		if got, err := Parse(append(strings.Fields(tc.args), id)); err != nil || got != tc.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
}

func TestCRIOBundle(t *testing.T) {
	layer := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, kind, container, root string
		persist, invalid, redirect  bool
	}{
		{"VFS rejected", "container", "app", "/storage/vfs/dir/" + layer, false, true, false},
		{"sandbox", "sandbox", "POD", "/storage/overlay/" + layer + "/merged", false, false, false},
		{"sidecar", "container", "sidecar", "/storage/overlay/" + layer + "/merged", false, false, false},
		{"overlay redirected", "container", "app", "/storage/overlay/" + layer + "/merged", true, false, true},
		{"unsupported root rejected", "container", "app", "/storage/overlay/" + layer + "/diff", false, true, false},
		{"relative root rejected", "container", "app", "vfs/dir/" + layer, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := t.TempDir()
			data, err := json.Marshal(map[string]any{
				"root": map[string]string{"path": tc.root},
				"annotations": map[string]string{
					"io.kubernetes.cri-o.ContainerType": tc.kind,
					"io.kubernetes.cri-o.SandboxName":   "k8s_pod_lab_app-0_uid_0",
					"io.kubernetes.pod.namespace":       "lab",
					"io.kubernetes.pod.name":            "app-0",
					"io.kubernetes.pod.uid":             "pod-uid",
					"io.kubernetes.container.name":      tc.container,
					"permabox.io/container":             "app",
					"permabox.io/persist":               "true",
					"permabox.io/underlying-runtime":    "runc",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			options, err := InspectBundle(bundle, "permabox.io/persist")
			if tc.invalid {
				if !IsInvalidPersistence(err) {
					t.Fatalf("want invalid persistence error, got %+v, %v", options, err)
				}
				return
			}
			if err != nil || (options.Persistence != nil) != tc.persist || options.RedirectRoot != tc.redirect {
				t.Fatalf("got %+v, %v; want persistence=%v", options, err, tc.persist)
			}
			if tc.kind == "sandbox" && options.RuntimeAlias != "" {
				t.Fatalf("sandbox selected runtime alias %q", options.RuntimeAlias)
			}
			if tc.persist {
				want := Bundle{tc.root, "lab", "app-0", "app", "pod-uid"}
				if got := *options.Persistence; got != want {
					t.Errorf("got %+v, want %+v", got, want)
				}
			}
		})
	}
}
