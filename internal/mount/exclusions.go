//go:build linux

package mount

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	lower := filepath.Join(alias, item.Path)
	return "lowerdir=" + lower + ",upperdir=" + filepath.Join(item.Scratch, "upper") + ",workdir=" + filepath.Join(item.Scratch, "work")
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
