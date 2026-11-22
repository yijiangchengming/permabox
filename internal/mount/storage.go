//go:build linux

package mount

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/yijiangchengming/permabox/internal/oci"
	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

// JSON can encode a byte in the OCI name as a six-byte escape.
const maxContainerRecordSize = 6 * oci.MaxBundleConfigSize

func storageIdentity(info os.FileInfo) slotIdentity {
	stat := info.Sys().(*syscall.Stat_t)
	return slotIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}
}

func openRecordedSlot(state record) (*os.File, error) {
	slot, err := openStorageSlot(state.Slot, false)
	if err != nil || state.SlotIdentity == nil {
		return slot, err
	}
	info, err := slot.Stat()
	if err == nil && storageIdentity(info) == *state.SlotIdentity {
		return slot, nil
	}
	slot.Close()
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("storage directory %s no longer matches its mount journal", state.Slot)
}

func openStorageSlot(slot string, create bool) (*os.File, error) {
	if !filepath.IsAbs(slot) || filepath.Clean(slot) != slot {
		return nil, errors.New("storage slot must have a clean absolute path")
	}
	namespace := filepath.Dir(slot)
	root, err := storageDir(trustedpath.OpenDir(filepath.Dir(namespace), create, false))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parent, err := storageDir(trustedpath.OpenChild(root, filepath.Base(namespace), create, false))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return storageDir(trustedpath.OpenChild(parent, filepath.Base(slot), create, false))
}

func storageDir(dir *os.File, err error) (*os.File, error) {
	if err != nil {
		return nil, err
	}
	info, err := dir.Stat()
	if err != nil {
		dir.Close()
		return nil, err
	}
	if info.Mode().Perm()&0022 != 0 {
		dir.Close()
		return nil, fmt.Errorf("%w: storage directory %s is writable by non-owners", trustedpath.ErrUnsafeDir, dir.Name())
	}
	return dir, nil
}

func readStorageRecord(path string, out any) error {
	return readRecordFile(path, 4096, out)
}

func readContainerRecord(path string) (string, error) {
	var name *string
	if err := readRecordFile(path, maxContainerRecordSize, &name); err != nil {
		return "", err
	}
	if name == nil {
		return "", fmt.Errorf("storage container marker %s must be a JSON string", path)
	}
	return *name, nil
}

func readRecordFile(path string, maxSize int64, out any) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	uid := int(info.Sys().(*syscall.Stat_t).Uid)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || (uid != 0 && uid != os.Geteuid()) {
		return fmt.Errorf("untrusted record file %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxSize {
		return fmt.Errorf("record file %s exceeds %d bytes", path, maxSize)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
