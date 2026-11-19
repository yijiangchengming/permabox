package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Invocation struct {
	Command, ID, Bundle string
}

// Flag tables describe the runc-compatible CLI; the original argv is never
// rewritten. Unrecognized forms remain the underlying runtime's responsibility.
var globalValue = set("--log", "--log-format", "--root", "--criu", "--rootless")
var globalBool = set("--debug", "--systemd-cgroup")
var createValue = set("--bundle", "-b", "--console-socket", "--pidfd-socket", "--pid-file", "--preserve-fds")
var createBool = set("--no-pivot", "--no-new-keyring", "--no-subreaper", "--keep")
var deleteBool = set("--force", "-f")
var execValue = set("--console-socket", "--cwd", "--env", "-e", "--pid-file", "--pidfd-socket", "--preserve-fds", "--process", "-p", "--user", "-u", "--additional-gids", "--cap", "--apparmor", "--process-label", "--cgroup", "--lsm-label")
var execBool = set("--detach", "-d", "--tty", "-t", "--no-new-privs", "--ignore-paused")
var updateValue = set("--resources", "-r", "--blkio-weight", "--cpu-period", "--cpu-quota", "--cpu-share", "--cpu-rt-period", "--cpu-rt-runtime", "--cpuset-cpus", "--cpuset-mems", "--memory", "--memory-reservation", "--memory-swap", "--pids-limit", "--l3-cache-schema", "--mem-bw-schema")
var checkpointValue = set("--image-path", "--work-path", "--parent-path", "--status-fd", "--manage-cgroups-mode", "--empty-ns", "--page-server")
var checkpointBool = set("--leave-running", "--tcp-established", "--tcp-skip-in-flight", "--ext-unix-sk", "--shell-job", "--file-locks", "--pre-dump", "--lazy-pages", "--auto-dedup")

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, value := range values {
		m[value] = true
	}
	return m
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_+.-]{1,240}$`)
var podPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func ValidID(id string) bool {
	return idPattern.MatchString(id) && id != "." && id != ".."
}

func Parse(args []string) Invocation {
	commandAt := 0
	for commandAt < len(args) && strings.HasPrefix(args[commandAt], "-") {
		if args[commandAt] == "--" {
			commandAt++
			break
		}
		end, _, _, ok := flag(args, commandAt, globalValue, globalBool)
		if !ok {
			return Invocation{}
		}
		commandAt = end
	}
	if commandAt >= len(args) {
		return Invocation{}
	}
	inv := Invocation{Command: args[commandAt]}
	var values, switches map[string]bool
	switch inv.Command {
	case "create":
		values, switches = createValue, createBool
		inv.Bundle = "."
	case "delete":
		switches = deleteBool
	case "start", "state", "pause", "resume":
	case "kill":
		switches = set("--all", "-a")
	case "exec":
		values, switches = execValue, execBool
	case "update":
		values = updateValue
	case "ps":
		values = set("--format", "-f")
	case "events":
		values, switches = set("--interval"), set("--stats")
	case "checkpoint":
		values, switches = checkpointValue, checkpointBool
	default:
		return Invocation{}
	}
	i := commandAt + 1
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if args[i] == "--" {
			i++
			break
		}
		end, name, value, ok := flag(args, i, values, switches)
		if !ok {
			return Invocation{}
		}
		if (name == "--bundle" || name == "-b") && value != "" {
			inv.Bundle = value
		}
		i = end
	}
	if i >= len(args) || !ValidID(args[i]) {
		return Invocation{}
	}
	if (inv.Command == "kill" && i+2 < len(args)) || (inv.Command != "exec" && inv.Command != "kill" && inv.Command != "ps" && i+1 != len(args)) {
		return Invocation{}
	}
	inv.ID = args[i]
	return inv
}

func flag(args []string, i int, values, switches map[string]bool) (int, string, string, bool) {
	name, value, hasValue := strings.Cut(args[i], "=")
	if switches[name] {
		return i + 1, name, value, true
	}
	if values[name] {
		if hasValue {
			return i + 1, name, value, true
		}
		if i+1 < len(args) {
			return i + 2, name, args[i+1], true
		}
	}
	return i, name, "", false
}

type Bundle struct {
	Root, Namespace, Pod, Container, PodUID string
	ReusePolicy                             string
}

const (
	StorageRootAnnotation = "permabox.io/storage-root"
	RuntimeAnnotation     = "permabox.io/underlying-runtime"
)

type BundleOptions struct {
	Persistence                *Bundle
	StorageAlias, RuntimeAlias string
}

type invalidPersistence struct{ error }

func IsInvalidPersistence(err error) bool {
	var invalid invalidPersistence
	return errors.As(err, &invalid)
}

func InspectBundle(bundle, annotation string) (BundleOptions, error) {
	var options BundleOptions
	data, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return options, err
	}
	var spec struct {
		Root struct {
			Path string `json:"path"`
		} `json:"root"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return options, fmt.Errorf("decode OCI config: %w", err)
	}
	a := spec.Annotations
	name := a["io.kubernetes.cri.container-name"]
	if a["io.kubernetes.cri.container-type"] == "sandbox" {
		return options, nil
	}
	options.RuntimeAlias = a[RuntimeAnnotation]
	if a[annotation] != "true" || (a["permabox.io/container"] != "" && a["permabox.io/container"] != name) {
		return options, nil
	}
	ns, pod := a["io.kubernetes.cri.sandbox-namespace"], a["io.kubernetes.cri.sandbox-name"]
	if ns == "" && pod == "" {
		ns, pod = a["io.kubernetes.pod.namespace"], a["io.kubernetes.pod.name"]
	}
	if !podPattern.MatchString(ns) || !podPattern.MatchString(pod) {
		return options, invalidPersistence{errors.New("persistence requires valid Pod namespace and name annotations")}
	}
	if spec.Root.Path == "" {
		return options, invalidPersistence{errors.New("persistence requires OCI root.path")}
	}
	root := spec.Root.Path
	if !filepath.IsAbs(root) {
		root = filepath.Join(bundle, root)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return options, invalidPersistence{err}
	}
	options.Persistence = &Bundle{root, ns, pod, name, a["io.kubernetes.cri.sandbox-uid"], a["permabox.io/reuse-policy"]}
	options.StorageAlias = a[StorageRootAnnotation]
	return options, nil
}
