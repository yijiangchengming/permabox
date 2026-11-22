//go:build linux

package mount

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type entry struct {
	ID, Parent             int
	Target, Kind, Options  string
	Device, FSRoot, Source string
	RawOptions             string
}

type system interface {
	namespace() (string, error)
	mounts() ([]entry, error)
	mount(source, target, kind string, flags uintptr, opts string) error
	unmount(target string, flags int) error
}

type linux struct{}

func (linux) namespace() (string, error) { return os.Readlink("/proc/self/ns/mnt") }
func (linux) mount(src, target, kind string, flags uintptr, opts string) error {
	return syscall.Mount(src, target, kind, flags, opts)
}
func (linux) unmount(target string, flags int) error { return syscall.Unmount(target, flags) }
func (linux) mounts() ([]entry, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readMounts(f)
}

var unescape = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func readMounts(r io.Reader) ([]entry, error) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 1024*1024)
	var mounts []entry
	for s.Scan() {
		before, after, ok := strings.Cut(s.Text(), " - ")
		left, right := strings.Fields(before), strings.Fields(after)
		if !ok || len(left) < 6 || len(right) < 3 {
			return nil, fmt.Errorf("malformed mountinfo entry")
		}
		id, err := strconv.Atoi(left[0])
		if err != nil {
			return nil, err
		}
		parent, err := strconv.Atoi(left[1])
		if err != nil {
			return nil, err
		}
		mount := entry{
			ID: id, Parent: parent, Device: left[2], FSRoot: unescape.Replace(left[3]),
			Target: unescape.Replace(left[4]), Kind: right[0], Source: unescape.Replace(right[1]),
			Options: unescape.Replace(right[2]), RawOptions: right[2],
		}
		// nsfs uses a namespace identifier such as net:[4026532292] as its root.
		if !filepath.IsAbs(mount.Target) || (!filepath.IsAbs(mount.FSRoot) && mount.Kind != "nsfs") {
			return nil, fmt.Errorf("malformed mountinfo paths: type %q, root %q, target %q", mount.Kind, mount.FSRoot, mount.Target)
		}
		mounts = append(mounts, mount)
	}
	return mounts, s.Err()
}

func mountReferences(mounts []entry, slot string) bool {
	for _, mount := range mounts {
		if within(slot, mount.Target) || within(slot, mount.Source) {
			return true
		}
		if mount.Kind == "overlay" {
			options := mount.RawOptions
			if options == "" {
				options = mount.Options
			}
			for _, option := range strings.Split(options, ",") {
				name, value, _ := strings.Cut(option, "=")
				switch name {
				case "lowerdir", "lowerdir+", "datadir+", "upperdir", "workdir":
					for _, path := range layerPaths(name, value) {
						if filepath.IsAbs(path) && overlap(slot, path) {
							return true
						}
					}
				}
			}
		}
	}
	// A bind mount can refer to the slot without naming it as its source.
	// Translate the slot into each covering mount's filesystem coordinates.
	for _, covering := range mounts {
		if covering.Device == "" || !within(covering.Target, slot) {
			continue
		}
		slotRoot := filepath.Join(covering.FSRoot, strings.TrimPrefix(slot, covering.Target))
		for _, mount := range mounts {
			if mount.ID != covering.ID && mount.Device == covering.Device && within(slotRoot, mount.FSRoot) {
				return true
			}
		}
	}
	return false
}

var layerEscape = strings.NewReplacer(`\134:`, `\072`, `\:`, `\072`)
var layerUnescape = strings.NewReplacer(`\072`, ":", `\054`, ",")

func layerPaths(name, value string) []string {
	paths := []string{value}
	if name == "lowerdir" {
		// A legacy lowerdir list uses colons; escaped colons are part of a path.
		paths = strings.Split(layerEscape.Replace(value), ":")
	}
	for i := range paths {
		paths[i] = unescape.Replace(layerUnescape.Replace(paths[i]))
	}
	return paths
}
