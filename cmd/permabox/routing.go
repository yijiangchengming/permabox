//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yijiangchengming/permabox/internal/oci"
)

type routes string

type route struct {
	Runtime string
	Bundle  string
}

func (r routes) path(id string) string { return filepath.Join(string(r), id+".runtime") }

func (r routes) lookup(id string) (route, error) {
	var rec route
	data, err := os.ReadFile(r.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return route{}, err
	}
	if !filepath.IsAbs(rec.Runtime) || !filepath.IsAbs(rec.Bundle) {
		return route{}, errors.New("invalid pinned runtime record")
	}
	return rec, nil
}

func (r routes) pin(id, runtime, bundle string) (bool, error) {
	bundle, err := filepath.Abs(bundle)
	if err != nil {
		return false, err
	}
	lock, err := r.lock(true)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	want := route{runtime, bundle}
	got, err := r.lookup(id)
	if err != nil {
		return false, err
	}
	if got.Runtime != "" {
		if got != want {
			return false, fmt.Errorf("container %s already pins a different runtime or bundle", id)
		}
		return false, nil
	}
	f, err := os.CreateTemp(string(r), ".runtime-")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if err := errors.Join(json.NewEncoder(f).Encode(want), f.Close()); err != nil {
		return false, err
	}
	err = os.Rename(f.Name(), r.path(id))
	return err == nil, err
}

func (r routes) forget(id string) error {
	lock, err := r.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	return r.remove(id)
}

func (r routes) remove(id string) error {
	err := os.Remove(r.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (r routes) sweep(now time.Time, managed func(string) (bool, error)) error {
	lock, err := r.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	entries, err := os.ReadDir(string(r))
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".runtime")
		if !ok || !oci.ValidID(id) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < 5*time.Minute {
			continue
		}
		if live, err := managed(id); err != nil || live {
			result = errors.Join(result, err)
			continue
		}
		rec, err := r.lookup(id)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if _, err := os.Lstat(rec.Bundle); errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, r.remove(id))
		}
	}
	return result
}

func (r routes) lock(create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(string(r), 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(string(r))
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("runtime state directory must be private")
	}
	f, err := os.Open(string(r))
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
