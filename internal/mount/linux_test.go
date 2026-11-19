//go:build linux

package mount

import (
	"strings"
	"testing"
)

func TestReadMountsWithNetworkNamespace(t *testing.T) {
	const mountInfo = "37 25 0:4 net:[4026532292] /run/netns/cni-test rw - nsfs nsfs rw\n"
	mounts, err := readMounts(strings.NewReader(mountInfo))
	if err != nil || len(mounts) != 1 || mounts[0].Target != "/run/netns/cni-test" {
		t.Fatalf("read network namespace mount: %v, %v", mounts, err)
	}
	if !mountReferences(mounts, "/run/netns") {
		t.Fatal("network namespace mount target lost from cleanup checks")
	}
	if _, err := readMounts(strings.NewReader(strings.Replace(mountInfo, " - nsfs ", " - ext4 ", 1))); err == nil {
		t.Fatal("accepted a nonabsolute filesystem root outside nsfs")
	}
}
