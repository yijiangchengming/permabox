//go:build linux

package mount

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yijiangchengming/permabox/internal/oci"
	"github.com/yijiangchengming/permabox/internal/ownership"
)

const slotOwnerMarker = ".permabox-owner.json"
const useMarker = ".permabox-use.json"

type slotUse struct {
	Boot      string `json:"boot"`
	ID        string `json:"container_id"`
	Confirmed bool   `json:"confirmed,omitempty"`
	Fresh     bool   `json:"fresh,omitempty"`
}

func bootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("empty kernel boot ID")
	}
	return id, nil
}

func beginSlotUse(slot, id string, fresh bool) error {
	boot, err := bootID()
	if err != nil {
		return err
	}
	if err := writeRecord(filepath.Join(slot, useMarker), slotUse{Boot: boot, ID: id, Fresh: fresh}); err != nil {
		return err
	}
	// Older runtimes do not check the use marker during rollback.
	return removeDurable(filepath.Join(slot, workloadMarker))
}

func confirmSlotUse(slot, id string, owner ownership.Owner) error {
	path := filepath.Join(slot, useMarker)
	var use slotUse
	if !readMarker(path, &use) {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return writeRecord(filepath.Join(slot, workloadMarker), owner) // Legacy journal.
		}
		return errors.New("invalid writable layer use marker")
	}
	if use.ID != id || use.Boot == "" {
		return errors.New("writable layer use does not match runtime journal")
	}
	if err := writeRecord(filepath.Join(slot, workloadMarker), owner); err != nil {
		return err
	}
	use.Confirmed = true
	use.Fresh = false
	return writeRecord(path, use)
}

// Called only for a slot with no known journal or mount references.
func recoverSlotRelease(slot string, now time.Time, currentBoot func() (string, error)) (bool, error) {
	path := filepath.Join(slot, useMarker)
	if _, err := os.Lstat(path); err != nil {
		return errors.Is(err, os.ErrNotExist), nil
	}
	var use slotUse
	if !readMarker(path, &use) || use.Boot == "" || !oci.ValidID(use.ID) {
		return false, nil
	}
	marker := filepath.Join(slot, releasedMarker)
	if !use.Confirmed {
		var released time.Time
		if !use.Fresh || !readMarker(marker, &released) || released.IsZero() {
			return false, nil
		}
		// Even a failed start may have written data. Only an empty upper from
		// a slot with no prior confirmed use is eligible for idle TTL.
		if _, err := os.Lstat(filepath.Join(slot, workloadMarker)); !errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		upper, err := openRealDir(filepath.Join(slot, "upper"))
		if err != nil {
			return false, nil
		}
		defer upper.Close()
		_, err = upper.Readdirnames(1)
		return errors.Is(err, io.EOF), nil
	}
	var owner ownership.Owner
	if !readMarker(filepath.Join(slot, workloadMarker), &owner) || !owner.Valid() {
		return false, nil
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		return err == nil, err
	}
	boot, err := currentBoot()
	if err != nil || use.Boot == boot {
		return false, err
	}
	if err := writeRecord(marker, now); err != nil {
		return false, err
	}
	return true, nil
}

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
	owner.Excludes = nil
	return writeRecord(path, owner)
}
