package oci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCRIEndpoint = "unix:///run/k3s/containerd/containerd.sock"

func TestContainerImageID(t *testing.T) {
	id := strings.Repeat("a", 64)
	digest := "sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, statusID, ref, want string
	}{
		{"raw digest", id, digest, digest},
		{"repository digest", id, "registry.example:5000/team/app@" + digest, "registry.example:5000/team/app@" + digest},
		{"wrong container", strings.Repeat("c", 64), digest, ""},
		{"short response ID", id[:12], digest, ""},
		{"mutable tag", id, "registry.example/app:latest", ""},
		{"short digest", id, "registry.example/app@sha256:abcd", ""},
		{"malformed reference", id, "invalid repo@" + digest, ""},
		{"missing imageRef", id, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"status":{"id":%q,"imageRef":%q,"image":{"image":%q}},"info":{"imageRef":%q}}`, tc.statusID, tc.ref, "repo/app@"+digest, digest)
			bin := fakeK3s(t, fmt.Sprintf(`
[ "$#" -eq 8 ] && [ "$1" = crictl ] && [ "$2" = --runtime-endpoint ] && [ "$3" = '%s' ] &&
[ "$4" = inspect ] && [ "$5" = --quiet ] && [ "$6" = --output ] && [ "$7" = json ] && [ "$8" = '%s' ] || exit 42
printf '%%s\n' '%s'
`, testCRIEndpoint, id, body))
			got, err := ContainerImageID(context.Background(), bin, testCRIEndpoint, id)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestContainerImageIDRejectsInvalidLookup(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, tc := range []struct{ name, script, endpoint, id, wantErr string }{
		{"invalid JSON", "printf broken", testCRIEndpoint, id, "decode"},
		{"oversized output", "head -c 2097152 /dev/zero", testCRIEndpoint, id, "output exceeds"},
		{"failed command", "exit 9", testCRIEndpoint, id, "exit status 9"},
		{"abbreviated ID", "exit 42", testCRIEndpoint, id[:12], "full container ID"},
		{"remote endpoint", "exit 42", "tcp://127.0.0.1:1234", id, "Unix socket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ContainerImageID(context.Background(), fakeK3s(t, tc.script), tc.endpoint, tc.id)
			if got != "" || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %q, %v; want error containing %q", got, err, tc.wantErr)
			}
		})
	}
}

func TestContainerImageIDDeadline(t *testing.T) {
	bin := fakeK3s(t, "exec sleep 10")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, err := ContainerImageID(ctx, bin, testCRIEndpoint, strings.Repeat("a", 64))
	if got != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %q, %v; want deadline exceeded", got, err)
	}
}

func fakeK3s(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake k3s")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}
