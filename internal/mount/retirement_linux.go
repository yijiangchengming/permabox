//go:build linux

package mount

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

func directoryMountID(dir *os.File) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", dir.Fd()))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if id, ok := strings.CutPrefix(line, "mnt_id:"); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id), nil
		}
	}
	return "", errors.New("mount identity is unavailable")
}

func removeRetiredContents(dir *os.File, mountID string) error {
	current, err := directoryMountID(dir)
	if err != nil {
		return err
	}
	if current != mountID {
		return fmt.Errorf("refusing to enter a mount at %s", dir.Name())
	}
	for {
		entries, err := dir.ReadDir(128)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := removeRetiredBatch(dir, mountID, entries); err != nil {
			return err
		}
	}
}

func removeRetiredBatch(dir *os.File, mountID string, entries []os.DirEntry) error {
	for _, entry := range entries {
		path := filepath.Join(trustedpath.FDPath(dir), entry.Name())
		if entry.IsDir() {
			fd, err := syscall.Openat(int(dir.Fd()), entry.Name(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			child := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), entry.Name()))
			if err := errors.Join(removeRetiredContents(child, mountID), child.Close()); err != nil {
				return err
			}
		}
		if err := remove(path); err != nil {
			return err
		}
	}
	return nil
}
