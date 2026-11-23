// Package ownership defines the metadata persisted with a writable layer.
package ownership

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

type Owner struct {
	Namespace string    `json:"namespace"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	UID       string    `json:"uid"`
	Overrides Overrides `json:"gc_overrides,omitzero"`
	Excludes  []string  `json:"persist_exclude,omitempty"`
}

type Overrides struct {
	Delete string `json:"delete,omitempty"`
	Zero   string `json:"zero,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func ValidName(name string) bool { return namePattern.MatchString(name) }

func ParseExclusions(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	seen := make(map[string]bool)
	paths := strings.Split(value, ",")
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "/" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
			return nil, errors.New("persist-exclude paths must be normalized absolute paths below the container root")
		}
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	return result, nil
}

func (o Owner) Valid() bool {
	return ValidName(o.Namespace) && ValidName(o.Name) && o.UID != "" && o.Kind == "StatefulSet"
}

func (o Owner) SameWorkload(other Owner) bool {
	return o.Valid() && other.Valid() && o.Namespace == other.Namespace && o.Kind == other.Kind && o.Name == other.Name && o.UID == other.UID
}
