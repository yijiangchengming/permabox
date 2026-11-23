//go:build linux

package mount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yijiangchengming/permabox/internal/oci"
)

func (m Manager) redirectTarget(id string) string {
	return filepath.Join(m.Dir, id+".rootfs")
}

func (r record) lowerSource() string {
	if r.Source != "" {
		return r.Source
	}
	return r.Root
}

// RedirectedRoot returns the original OCI root for a retry in the same bundle.
func (m Manager) RedirectedRoot(id, bundle string) (target, source string, err error) {
	if !oci.ValidID(id) {
		return "", "", errors.New("invalid container ID")
	}
	lock, err := m.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	defer lock.Close()
	rec, err := readRecord(lock.journal(id))
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil || rec.Source == "" {
		return "", "", err
	}
	bundle, err = filepath.Abs(bundle)
	if err != nil {
		return "", "", err
	}
	if rec.Root != m.redirectTarget(id) || rec.Bundle != bundle || !rec.ReleasedAt.IsZero() {
		return "", "", errors.New("redirected rootfs journal must be cleaned up before create")
	}
	return rec.Root, rec.Source, nil
}

func (m Manager) overlaySourceID(source string) (int, error) {
	mounts, err := m.sys.mounts()
	if err != nil {
		return 0, err
	}
	var id int
	for _, mount := range mounts {
		if mount.Target != source {
			continue
		}
		if id != 0 || mount.Kind != "overlay" || mount.FSRoot != "/" {
			return 0, fmt.Errorf("CRI-O source %s must be a single native OverlayFS mount", source)
		}
		id = mount.ID
	}
	if id == 0 {
		return 0, fmt.Errorf("CRI-O source %s is missing its native OverlayFS mount", source)
	}
	return id, nil
}

func (m lockedManager) publishBundleRoot(id string, state *record) error {
	if state.ConfigPending {
		_, err := oci.ReplaceBundleRoot(state.Bundle, state.Source, state.Root)
		return err
	}
	state.ConfigPending = true
	if err := writeRecord(m.journal(id), state); err != nil {
		state.ConfigPending = false
		return err
	}
	published, err := oci.ReplaceBundleRoot(state.Bundle, state.Source, state.Root)
	if err != nil && !published {
		state.ConfigPending = false
		return errors.Join(err, writeRecord(m.journal(id), state))
	}
	return err
}
