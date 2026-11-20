//go:build linux

package lifecycle

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yijiangchengming/permabox/internal/config"
	"github.com/yijiangchengming/permabox/internal/mount"
)

func TestPassthroughProcess(t *testing.T) {
	base := privateStateDir(t)
	out, stderr, code := call(t, base, "23", "--root", "a b", "state", "container-id")
	if out != "--root\na b\nstate\ncontainer-id\nforwarded\n" || stderr != "runtime stderr\n" || code != 23 {
		t.Fatalf("stdout=%q stderr=%q exit=%d", out, stderr, code)
	}
}

func TestManagedDeleteRetainsStateUntilSuccess(t *testing.T) {
	base := privateStateDir(t)
	journal := filepath.Join(base, "id.json")
	ns, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]string{"mount_namespace": ns, "root": filepath.Join(base, "rootfs"), "slot": filepath.Join(base, "store")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, exit := range []string{"17", "0", "0"} {
		out, stderr, code := call(t, base, exit, "delete", "--force", "id")
		if out != "delete\n--force\nid\nforwarded\n" || stderr != "runtime stderr\n" || (exit == "0" && code != 0) || (exit == "17" && code != 17) {
			t.Fatalf("stdout=%q stderr=%q exit=%d", out, stderr, code)
		}
		_, err := os.Stat(journal)
		if (exit == "17" && err != nil) || (exit == "0" && !os.IsNotExist(err)) {
			t.Fatalf("journal after %s: %v", exit, err)
		}
	}
}

func TestManagedStartCommitsOwnershipOnlyAfterSuccess(t *testing.T) {
	base := privateStateDir(t)
	slot := filepath.Join(base, "store", "default", "app-0")
	if err := os.MkdirAll(slot, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(slot, ".permabox-workload.json")
	oldOwner := `{"namespace":"default","kind":"StatefulSet","name":"app","uid":"old"}`
	newOwner := `{"namespace":"default","kind":"StatefulSet","name":"app","uid":"new"}`
	ns, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	journal, err := json.Marshal(map[string]any{
		"mount_namespace": ns, "root": filepath.Join(base, "rootfs"), "slot": slot,
		"pending_workload_owner": json.RawMessage(newOwner),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "id.json"), journal, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(oldOwner), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status string
		code   int
		owner  string
	}{{"17", 17, "old"}, {"0", 0, "new"}} {
		out, stderr, code := call(t, base, tc.status, "start", "id")
		if out != "start\nid\nforwarded\n" || stderr != "runtime stderr\n" || code != tc.code {
			t.Fatalf("start: stdout=%q stderr=%q exit=%d; want %d", out, stderr, code, tc.code)
		}
		var got struct{ UID string }
		data, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &got); err != nil || got.UID != tc.owner {
			t.Fatalf("owner after start exit %d = %q (%v); want %q", code, got.UID, err, tc.owner)
		}
	}
}

func TestRuntimeOverrideStaysPinnedUntilDelete(t *testing.T) {
	base := privateStateDir(t)
	bundle := filepath.Join(base, "bundle")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, data string) {
		if err := os.WriteFile(path, []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bundle, "config.json"), `{"annotations":{"permabox.io/underlying-runtime":"alternate"}}`)
	write(filepath.Join(base, "alternate"), "#!/bin/sh\nprintf 'alternate %s\\n' \"$1\"\nexit \"$TEST_STATUS\"\n")
	for _, tc := range []struct {
		command, status string
		code            int
	}{{"create", "0", 0}, {"start", "0", 0}, {"exec", "0", 0}, {"kill", "0", 0}, {"delete", "17", 17}, {"state", "0", 0}, {"delete", "0", 0}} {
		args := []string{tc.command, "test-id"}
		switch tc.command {
		case "create":
			args = []string{"create", "--bundle", bundle, "test-id"}
		case "exec":
			args = []string{"exec", "--process", "/tmp/process.json", "test-id"}
		case "kill":
			args = append(args, "9")
		}
		out, stderr, code := call(t, base, tc.status, args...)
		if out != "alternate "+tc.command+"\n" || stderr != "" || code != tc.code {
			t.Fatalf("%s: %q %q %d", tc.command, out, stderr, code)
		}
	}
	out, _, _ := call(t, base, "0", "state", "test-id")
	if out != "state\ntest-id\nforwarded\n" {
		t.Fatalf("after delete: %q", out)
	}
	write(filepath.Join(bundle, "config.json"), `{"annotations":{"permabox.io/underlying-runtime":"/usr/bin/unsafe"}}`)
	out, stderr, code := call(t, base, "0", "create", "--bundle", bundle, "new-id")
	if out != "" || stderr != "" || code != 1 {
		t.Fatalf("unknown alias: %q %q %d", out, stderr, code)
	}
}

func TestDefaultSandboxRuntimeStaysPinnedAfterConfigChange(t *testing.T) {
	base := privateStateDir(t)
	bundle := filepath.Join(base, "bundle")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), []byte(`{"annotations":{"io.kubernetes.cri.container-type":"sandbox"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := call(t, base, "0", "create", "-b", bundle, "sandbox-id"); code != 0 {
		t.Fatalf("create: %s (%d)", stderr, code)
	}
	if err := os.WriteFile(filepath.Join(base, "changed-default"), []byte("#!/bin/sh\nprintf 'new runtime\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out, _, code := call(t, base, "0", "state", "sandbox-id")
	if code != 0 || out != "state\nsandbox-id\nforwarded\n" {
		t.Fatalf("state used changed default: %q (%d)", out, code)
	}
}

func privateStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func call(t *testing.T, base, exit string, args ...string) (string, string, int) {
	t.Helper()
	runtime := filepath.Join(base, "fake-runtime")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" \"$TEST_FORWARD\"\nprintf '%s\\n' 'runtime stderr' >&2\nexit \"$TEST_STATUS\"\n"
	if err := os.WriteFile(runtime, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestWrapperHelper$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "TEST_ROOT="+base, "TEST_FORWARD=forwarded", "TEST_STATUS="+exit)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if status, ok := err.(*exec.ExitError); ok {
			return out.String(), stderr.String(), status.ExitCode()
		}
		t.Fatal(err)
	}
	return out.String(), stderr.String(), 0
}

func TestWrapperHelper(t *testing.T) {
	base := os.Getenv("TEST_ROOT")
	if base == "" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if strings.Compare(arg, "--") == 0 {
			args = os.Args[i+1:]
			break
		}
	}
	cfg := config.Config{StorageRoot: filepath.Join(base, "store"), Runtime: filepath.Join(base, "fake-runtime"), Annotation: "permabox.io/persist"}
	cfg.UnderlyingRuntimes = map[string]string{"alternate": filepath.Join(base, "alternate")}
	changed := filepath.Join(base, "changed-default")
	if _, err := os.Stat(changed); err == nil {
		cfg.Runtime = changed
	}
	os.Exit(Run(cfg, mount.New(base), args, t.Logf))
}
