// Package ownership defines the metadata persisted with a writable layer.
package ownership

import "regexp"

type Owner struct {
	Namespace string    `json:"namespace"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	UID       string    `json:"uid"`
	Overrides Overrides `json:"gc_overrides,omitzero"`
}

type Overrides struct {
	Delete string `json:"delete,omitempty"`
	Zero   string `json:"zero,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func ValidName(name string) bool { return namePattern.MatchString(name) }

func (o Owner) Valid() bool {
	return ValidName(o.Namespace) && ValidName(o.Name) && o.UID != "" && o.Kind == "StatefulSet"
}

func (o Owner) SameWorkload(other Owner) bool {
	return o.Valid() && other.Valid() && o.Namespace == other.Namespace && o.Kind == other.Kind && o.Name == other.Name && o.UID == other.UID
}
