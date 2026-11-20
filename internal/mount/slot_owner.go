//go:build linux

package mount

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/yijiangchengming/permabox/internal/ownership"
)

const slotOwnerMarker = ".permabox-owner.json"

func bindSlotOwner(slot string, owner ownership.Owner) error {
	path := filepath.Join(slot, slotOwnerMarker)
	var saved ownership.Owner
	if readMarker(path, &saved) {
		if !saved.SameWorkload(owner) {
			return errors.New("writable layer belongs to a different or unverified StatefulSet")
		}
		return nil
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("writable layer has an unreadable identity marker")
	}
	if readMarker(filepath.Join(slot, workloadMarker), &saved) && saved.Valid() {
		if !saved.SameWorkload(owner) {
			return errors.New("legacy writable layer belongs to a different or unverified StatefulSet")
		}
	} else if owner.Valid() {
		entries, err := os.ReadDir(slot)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("legacy writable layer has no verified StatefulSet identity; archive or migrate it before reuse")
		}
	}
	if !owner.Valid() {
		return nil // Only the low-level mount API can prepare an unowned layer.
	}
	owner.Overrides = ownership.Overrides{}
	return writeRecord(path, owner)
}
