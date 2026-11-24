//go:build linux

package mount

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

func resolveExclusionDirectories(root string, paths []string) ([]string, error) {
	var resolved []string
	for _, requested := range paths {
		if !filepath.IsAbs(requested) || filepath.Clean(requested) != requested || requested == "/" {
			return nil, fmt.Errorf("invalid persistent directory exclusion %q", requested)
		}
		relative, source, err := resolveRootfsPath(root, requested)
		if err != nil {
			return nil, fmt.Errorf("resolve persistent directory exclusion %s: %w", requested, err)
		}
		if source == "" {
			return nil, fmt.Errorf("persistent directory exclusion %s does not exist in the image rootfs", requested)
		}
		info, err := os.Lstat(source)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("persistent exclusion %s does not resolve to a directory", requested)
		}
		for _, previous := range resolved {
			if overlap(previous, relative) {
				return nil, fmt.Errorf("persistent directory exclusions %s and %s overlap", previous, relative)
			}
		}
		if strings.ContainsAny(relative, ",:\\\n\r\t\x00") {
			return nil, fmt.Errorf("persistent directory exclusion %s contains characters unsupported by OverlayFS mount options", requested)
		}
		resolved = append(resolved, relative)
	}
	return resolved, nil
}

func resolveRootfsPath(root, requested string) (string, string, error) {
	remaining := strings.Split(strings.TrimPrefix(requested, string(filepath.Separator)), string(filepath.Separator))
	resolved := make([]string, 0, len(remaining))
	links := 0
	for len(remaining) > 0 {
		part := remaining[0]
		remaining = remaining[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", "", errors.New("path escapes the container root")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidateParts := make([]string, 0, len(resolved)+2)
		candidateParts = append(candidateParts, root)
		candidateParts = append(candidateParts, resolved...)
		candidateParts = append(candidateParts, part)
		candidate := filepath.Join(candidateParts...)
		info, err := os.Lstat(candidate)
		if err != nil {
			return "", "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", "", errors.New("too many symbolic links in persistent directory exclusion")
			}
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", "", err
			}
			if filepath.IsAbs(target) {
				resolved = resolved[:0]
			}
			remaining = append(strings.Split(target, string(filepath.Separator)), remaining...)
			continue
		}
		if len(remaining) > 0 && !info.IsDir() {
			return "", "", syscall.ENOTDIR
		}
		resolved = append(resolved, part)
	}
	relative := filepath.Join(resolved...)
	if relative == "." {
		return "", "", errors.New("the container root cannot be excluded")
	}
	return relative, filepath.Join(root, relative), nil
}

func exclusionOverlayOptions(alias string, item exclusionMount) string {
	return exclusionOverlayLayers(alias, item) + ",index=off,metacopy=off"
}

func exclusionOverlayLayers(alias string, item exclusionMount) string {
	return "lowerdir=" + filepath.Join(alias, item.Path) + ",upperdir=" + filepath.Join(item.Scratch, "upper") + ",workdir=" + filepath.Join(item.Scratch, "work")
}

const exclusionPoolSource = "permabox-exclusions"
const exclusionPoolOptions = "mode=0700,size=4096m,nr_inodes=4096"

func (m Manager) exclusionPool(id string) string {
	return filepath.Join(m.Dir, id+".excludes")
}

func (m lockedManager) installExclusionPool(id string, state *record) error {
	path := state.ExclusionPool
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	if err := m.sys.mount(exclusionPoolSource, path, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, exclusionPoolOptions); err != nil {
		return fmt.Errorf("mount temporary exclusion filesystem: %w", err)
	}
	if err := m.sys.mount("", path, "", syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make temporary exclusion filesystem private: %w", err)
	}
	mounts, err := m.sys.mounts()
	if err != nil {
		return err
	}
	state.ExclusionPoolID, err = findExclusionPool(mounts, *state)
	if err != nil {
		return err
	}
	if state.ExclusionPoolID == 0 {
		return errors.New("temporary exclusion filesystem is missing from mountinfo")
	}
	return writeRecord(m.journal(id), state)
}

func findExclusionPool(mounts []entry, state record) (int, error) {
	var id int
	for _, mount := range mounts {
		if mount.Target != state.ExclusionPool {
			if state.ExclusionPoolID != 0 && mount.ID == state.ExclusionPoolID {
				return 0, errors.New("recorded temporary exclusion filesystem moved")
			}
			continue
		}
		if id != 0 || mount.Kind != "tmpfs" || mount.Source != exclusionPoolSource || mount.FSRoot != "/" ||
			(state.ExclusionPoolID != 0 && mount.ID != state.ExclusionPoolID) {
			return 0, fmt.Errorf("unexpected mount at temporary exclusion filesystem %s", state.ExclusionPool)
		}
		id = mount.ID
	}
	return id, nil
}

func (m lockedManager) removeExclusionPool(id string, state record, mounts []entry) error {
	path := state.ExclusionPool
	if path != m.exclusionPool(id) {
		return errors.New("invalid temporary exclusion filesystem path in mount journal")
	}
	poolID, err := findExclusionPool(mounts, state)
	if err != nil {
		return err
	}
	if poolID != 0 {
		index := slices.IndexFunc(mounts, func(mount entry) bool { return mount.ID == poolID })
		device := mounts[index].Device
		mounts = slices.Delete(mounts, index, index+1)
		if device != "" && slices.ContainsFunc(mounts, func(mount entry) bool { return mount.Device == device }) {
			return fmt.Errorf("temporary exclusion filesystem %s has another mount", path)
		}
	}
	if mountReferences(mounts, path) {
		return fmt.Errorf("temporary exclusion filesystem %s is still referenced by a mount", path)
	}
	if poolID != 0 {
		if err := realDir(path); err != nil {
			return err
		}
		if err := m.sys.unmount(path, 0); err != nil {
			return fmt.Errorf("unmount temporary exclusion filesystem: %w", err)
		}
	}
	return remove(path)
}

func exclusionHash(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

func createExclusionScratch(path string, imageDirectory os.FileInfo) error {
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(path)
		}
	}()
	if err := initUpper(filepath.Join(path, "upper"), imageDirectory); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(path, "work"), 0700); err != nil {
		return err
	}
	complete = true
	return nil
}

func removeExclusionScratch(stateDir, id string, item exclusionMount) error {
	name := filepath.Base(item.Scratch)
	prefix := id + ".exclude-"
	if filepath.Dir(item.Scratch) != stateDir || !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+64 || name != prefix+exclusionHash(item.Path) {
		return errors.New("invalid temporary exclusion storage path in mount journal")
	}
	dir, err := trustedpath.OpenDir(item.Scratch, false, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	return os.RemoveAll(item.Scratch)
}
