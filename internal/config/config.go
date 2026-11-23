package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yijiangchengming/permabox/internal/trustedpath"
)

const (
	defaultFile = "/etc/permabox/config.yaml"
	DefaultLog  = "/var/log/permabox.log"
	StateDir    = "/run/permabox"
)

type Config struct {
	StateDir              string            `yaml:"state_dir"`
	StorageRoot           string            `yaml:"storage_root"`
	Runtime               string            `yaml:"underlying_runtime"`
	LogFile               string            `yaml:"log_file"`
	Annotation            string            `yaml:"persist_annotation"`
	TrustedNamespaces     []string          `yaml:"trusted_namespaces"`
	StorageRoots          map[string]string `yaml:"storage_roots"`
	UnderlyingRuntimes    map[string]string `yaml:"underlying_runtimes"`
	GCIdleTTL             time.Duration     `yaml:"gc_idle_ttl"`
	GCOnWorkloadDelete    bool              `yaml:"gc_on_workload_delete"`
	GCOnScaleToZero       bool              `yaml:"gc_on_scale_to_zero"`
	GCWorkloadAnnotations bool              `yaml:"gc_workload_annotations"`
	WorkloadSocket        string            `yaml:"workload_socket"`
}

var aliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
var namespacePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func Load() (Config, error) {
	cfg := Config{StateDir: StateDir, StorageRoot: "/var/lib/permabox/storage", Runtime: "/usr/bin/runc", LogFile: DefaultLog, Annotation: "permabox.io/persist"}
	path := os.Getenv("PERMABOX_CONFIG")
	optional := path == ""
	if optional {
		path = defaultFile
	}
	if err := read(path, &cfg); err != nil && !(optional && errors.Is(err, os.ErrNotExist)) {
		return cfg, fmt.Errorf("load %s: %w", path, err)
	}
	for _, option := range []struct {
		key string
		dst *string
	}{
		{"PERMABOX_STORAGE_ROOT", &cfg.StorageRoot},
		{"PERMABOX_UNDERLYING_RUNTIME", &cfg.Runtime},
		{"PERMABOX_LOG_FILE", &cfg.LogFile},
		{"PERMABOX_PERSIST_ANNOTATION", &cfg.Annotation},
	} {
		if value, ok := os.LookupEnv(option.key); ok {
			*option.dst = value
		}
	}
	if value, ok := os.LookupEnv("PERMABOX_GC_IDLE_TTL"); ok {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return cfg, fmt.Errorf("PERMABOX_GC_IDLE_TTL: %w", err)
		}
		cfg.GCIdleTTL = parsed
	}
	if cfg.GCIdleTTL < 0 {
		return cfg, errors.New("gc_idle_ttl must not be negative")
	}
	if !validPath(cfg.StateDir) || filepath.Clean(cfg.StateDir) != cfg.StateDir {
		return cfg, errors.New("state_dir must be a clean absolute path")
	}
	if cfg.WorkloadSocket != "" && (!validPath(cfg.WorkloadSocket) || filepath.Clean(cfg.WorkloadSocket) != cfg.WorkloadSocket) {
		return cfg, errors.New("workload_socket must be a clean absolute path")
	}
	for _, choices := range []map[string]string{cfg.StorageRoots, cfg.UnderlyingRuntimes} {
		for alias, path := range choices {
			if !aliasPattern.MatchString(alias) || !validPath(path) || filepath.Clean(path) != path {
				return cfg, fmt.Errorf("invalid override alias or path: %q = %q", alias, path)
			}
		}
	}
	for _, path := range []string{cfg.StorageRoot, cfg.Runtime, cfg.LogFile} {
		if !validPath(path) {
			return cfg, fmt.Errorf("invalid absolute config path %q", path)
		}
	}
	for _, namespace := range cfg.TrustedNamespaces {
		if !namespacePattern.MatchString(namespace) {
			return cfg, fmt.Errorf("trusted_namespaces: invalid DNS label %q", namespace)
		}
	}
	if strings.TrimSpace(cfg.Annotation) == "" {
		return cfg, errors.New("persist_annotation must not be empty")
	}
	return cfg, nil
}

func (c Config) TrustsNamespace(namespace string) bool {
	return slices.Contains(c.TrustedNamespaces, namespace)
}

func (c Config) WorkloadGC() bool {
	return c.GCOnWorkloadDelete || c.GCOnScaleToZero || c.GCWorkloadAnnotations
}

func validPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) != "/" && strings.IndexByte(path, 0) < 0
}

func (c Config) AllStorageRoots() []string {
	roots := []string{c.StorageRoot}
	for _, root := range c.StorageRoots {
		roots = append(roots, root)
	}
	return roots
}

func read(path string, cfg *Config) error {
	const maxSize = 1 << 20
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		path = cwd + string(os.PathSeparator) + path
	}
	f, err := trustedpath.OpenRegularFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxSize {
		return errors.New("node configuration exceeds 1 MiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("expected a single YAML document")
	}
	return nil
}
