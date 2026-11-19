package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOverridesAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("storage_root: /srv/yaml\nlog_file: '/tmp/perma log'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PERMABOX_CONFIG", path)
	t.Setenv("PERMABOX_STORAGE_ROOT", "/srv/env")
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.StorageRoot != "/srv/env" || got.LogFile != "/tmp/perma log" || got.Runtime != "/usr/bin/runc" || got.Annotation != "permabox.io/persist" {
		t.Fatalf("unexpected config: %+v", got)
	}
	t.Setenv("PERMABOX_CONFIG", path+".missing")
	if _, err := Load(); err == nil {
		t.Fatal("accepted an explicitly configured missing file")
	}
}

func TestLoadNamedOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("PERMABOX_CONFIG", path)
	if err := os.WriteFile(path, []byte("storage_roots: {fast: /disk/fast}\nunderlying_runtimes: {sysbox: /usr/bin/sysbox-runc}\ngc_idle_ttl: 24h\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorageRoots["fast"] != "/disk/fast" || cfg.UnderlyingRuntimes["sysbox"] != "/usr/bin/sysbox-runc" || cfg.GCIdleTTL != 24*time.Hour {
		t.Fatalf("config: %+v", cfg)
	}
	if err := os.WriteFile(path, []byte("storage_roots: {bad: /disk/../etc}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("accepted unclean path")
	}
}
