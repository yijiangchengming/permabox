package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

type Invocation struct {
	Command, ID, Bundle string
}

// Flag tables describe the runc-compatible CLI; the original argv is never
// rewritten. Unsupported forms are rejected before runtime dispatch.
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
var crioVFSLayer = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidID(id string) bool {
	return idPattern.MatchString(id) && id != "." && id != ".."
}

func Parse(args []string) (Invocation, error) {
	commandAt := 0
	for commandAt < len(args) && strings.HasPrefix(args[commandAt], "-") {
		if args[commandAt] == "--" {
			commandAt++
			break
		}
		if commandAt == len(args)-1 && (args[commandAt] == "--help" || args[commandAt] == "-h" || args[commandAt] == "--version" || args[commandAt] == "-v") {
			return Invocation{}, nil
		}
		end, _, _, ok := flag(args, commandAt, globalValue, globalBool)
		if !ok {
			return Invocation{}, fmt.Errorf("unsupported OCI global option %q", args[commandAt])
		}
		commandAt = end
	}
	if commandAt >= len(args) {
		return Invocation{}, nil
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
	case "list", "features", "help", "spec":
		return Invocation{}, nil
	default:
		return Invocation{}, fmt.Errorf("unsupported OCI command %q", inv.Command)
	}
	i := commandAt + 1
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if args[i] == "--" {
			i++
			break
		}
		end, name, value, ok := flag(args, i, values, switches)
		if !ok {
			return Invocation{}, fmt.Errorf("%s: unsupported or incomplete OCI option %q", inv.Command, args[i])
		}
		if name == "--bundle" || name == "-b" {
			if value == "" {
				return Invocation{}, errors.New("create: empty OCI bundle path")
			}
			inv.Bundle = value
		}
		i = end
	}
	if i >= len(args) || !ValidID(args[i]) {
		return Invocation{}, fmt.Errorf("%s: missing or invalid container ID", inv.Command)
	}
	if (inv.Command == "kill" && i+2 < len(args)) || (inv.Command != "exec" && inv.Command != "kill" && inv.Command != "ps" && i+1 != len(args)) {
		return Invocation{}, fmt.Errorf("%s: unexpected OCI arguments after container ID", inv.Command)
	}
	inv.ID = args[i]
	return inv, nil
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
}

const (
	StorageRootAnnotation = "permabox.io/storage-root"
	RuntimeAnnotation     = "permabox.io/underlying-runtime"
)

type BundleOptions struct {
	Persistence                *Bundle
	StorageAlias, RuntimeAlias string
	Namespace                  string
}

type invalidPersistence struct{ error }

func IsInvalidPersistence(err error) bool {
	var invalid invalidPersistence
	return errors.As(err, &invalid)
}

func InspectBundle(bundle, annotation string) (BundleOptions, error) {
	var options BundleOptions
	data, err := readBundleConfig(bundle)
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
	crioType := a["io.kubernetes.cri-o.ContainerType"]
	if crioType == "sandbox" || a["io.kubernetes.cri.container-type"] == "sandbox" {
		return options, nil
	}
	name := a["io.kubernetes.cri.container-name"]
	if crioType == "container" {
		name = a["io.kubernetes.container.name"]
	}
	ns, pod, uid := a["io.kubernetes.cri.sandbox-namespace"], a["io.kubernetes.cri.sandbox-name"], a["io.kubernetes.cri.sandbox-uid"]
	if crioType == "container" || (ns == "" && pod == "") {
		ns, pod = a["io.kubernetes.pod.namespace"], a["io.kubernetes.pod.name"]
	}
	options.Namespace = ns
	options.RuntimeAlias = a[RuntimeAnnotation]
	if a[annotation] != "true" || (a["permabox.io/container"] != "" && a["permabox.io/container"] != name) {
		return options, nil
	}
	if crioType == "container" {
		uid = a["io.kubernetes.pod.uid"]
		if name == "" {
			return options, invalidPersistence{errors.New("CRI-O persistence requires the Kubernetes container name label")}
		}
		if !isCRIOVFSRoot(spec.Root.Path) {
			return options, invalidPersistence{errors.New("CRI-O persistence requires a containers/storage VFS rootfs")}
		}
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
	options.Persistence = &Bundle{root, ns, pod, name, uid}
	options.StorageAlias = a[StorageRootAnnotation]
	return options, nil
}

const MaxBundleConfigSize = 4 << 20

func readBundleConfig(bundle string) ([]byte, error) {
	if filepath.Clean(bundle) != bundle {
		return nil, errors.New("OCI bundle path must be clean")
	}
	path, err := filepath.Abs(bundle)
	if err != nil {
		return nil, err
	}
	dir, err := trustedpath.OpenDir(path, false, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	dirInfo, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	if dirInfo.Mode().Perm()&0022 != 0 {
		return nil, errors.New("OCI bundle directory must be writable only by its trusted owner")
	}
	fd, err := syscall.Openat(int(dir.Fd()), "config.json", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open OCI config: %w", err)
	}
	file := os.NewFile(uintptr(fd), "config.json")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	uid := int(info.Sys().(*syscall.Stat_t).Uid)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || (uid != 0 && uid != os.Geteuid()) {
		return nil, errors.New("OCI config must be a regular file writable only by its trusted owner")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBundleConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBundleConfigSize {
		return nil, errors.New("OCI config exceeds 4 MiB")
	}
	return data, nil
}

func isCRIOVFSRoot(root string) bool {
	parent := filepath.Dir(root)
	// Only VFS leaves the graph driver's rootfs alone during CRI StopContainer.
	return filepath.IsAbs(root) && filepath.Clean(root) == root &&
		filepath.Base(parent) == "dir" && filepath.Base(filepath.Dir(parent)) == "vfs" &&
		crioVFSLayer.MatchString(filepath.Base(root))
}
