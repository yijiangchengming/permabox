package workload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yijiangchengming/permabox/internal/ownership"
)

const (
	deleteAnnotation      = "permabox.io/gc-on-workload-delete"
	scaleToZeroAnnotation = "permabox.io/gc-on-scale-to-zero"
)

func readOverrides(annotations map[string]string) ownership.Overrides {
	read := func(key string) string {
		value, exists := annotations[key]
		if !exists {
			return ""
		}
		if value == "true" {
			return "true"
		}
		return "false"
	}
	return ownership.Overrides{Delete: read(deleteAnnotation), Zero: read(scaleToZeroAnnotation)}
}

func ApplyOverrides(o ownership.Overrides, onDelete, onZero bool) (bool, bool) {
	if o.Delete != "" {
		onDelete = o.Delete == "true"
	}
	if o.Zero != "" {
		onZero = o.Zero == "true"
	}
	return onDelete, onZero
}

type reference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type object struct {
	Metadata struct {
		UID               string            `json:"uid"`
		References        []reference       `json:"ownerReferences,omitempty"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
		Annotations       map[string]string `json:"annotations,omitempty"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas,omitempty"`
	} `json:"spec"`
}

type Client struct {
	base string
	http *http.Client
	// Read the projected token per request so rotation needs no restart.
	tokenPath string
}

var errNotFound = errors.New("Kubernetes object not found")

var collection = map[string]string{
	"Pod": "pods", "StatefulSet": "statefulsets",
}

func resourcePath(kind, namespace, name string) (string, error) {
	resource, ok := collection[kind]
	if !ok || !ownership.ValidName(namespace) || !ownership.ValidName(name) {
		return "", errors.New("invalid Kubernetes resource identity")
	}
	prefix := "/apis/apps/v1"
	if kind == "Pod" {
		prefix = "/api/v1"
	}
	return prefix + "/namespaces/" + namespace + "/" + resource + "/" + name, nil
}

func (c Client) get(ctx context.Context, kind, namespace, name string) (object, error) {
	var obj object
	path, err := resourcePath(kind, namespace, name)
	if err != nil {
		return obj, err
	}
	resp, err := c.request(ctx, path)
	if err != nil {
		return obj, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound {
		var status struct {
			Reason  string `json:"reason"`
			Details struct {
				Name string `json:"name"`
			} `json:"details"`
		}
		if dec.Decode(&status) == nil && status.Reason == "NotFound" && status.Details.Name == name {
			return obj, errNotFound
		}
	}
	if resp.StatusCode != http.StatusOK {
		return obj, fmt.Errorf("read %s %s/%s: HTTP %d", kind, namespace, name, resp.StatusCode)
	}
	if err := dec.Decode(&obj); err != nil {
		return obj, err
	}
	if obj.Metadata.UID == "" {
		return obj, errors.New("Kubernetes object response has no UID")
	}
	return obj, nil
}

func controller(obj object) reference {
	var owner reference
	for _, ref := range obj.Metadata.References {
		if ref.Controller {
			if owner.Controller || ref.APIVersion != "apps/v1" || ref.UID == "" || !ownership.ValidName(ref.Name) {
				return reference{}
			}
			owner = ref
		}
	}
	return owner
}

func (c Client) Resolve(ctx context.Context, namespace, pod, uid string) (ownership.Owner, error) {
	if uid == "" {
		return ownership.Owner{}, nil
	}
	obj, err := c.get(ctx, "Pod", namespace, pod)
	if errors.Is(err, errNotFound) {
		return ownership.Owner{}, nil
	}
	if err != nil || obj.Metadata.UID != uid {
		return ownership.Owner{}, err
	}
	ref := controller(obj)
	if ref.Kind != "StatefulSet" {
		return ownership.Owner{}, nil
	}
	set, err := c.get(ctx, "StatefulSet", namespace, ref.Name)
	if err != nil {
		return ownership.Owner{}, err
	}
	if set.Metadata.UID != ref.UID || set.Metadata.DeletionTimestamp != nil {
		return ownership.Owner{}, errors.New("Pod refers to a replaced or terminating StatefulSet")
	}
	return ownership.Owner{Namespace: namespace, Kind: ref.Kind, Name: ref.Name, UID: ref.UID,
		Overrides: readOverrides(obj.Metadata.Annotations)}, nil
}

func (c Client) Reclaim(ctx context.Context, owner ownership.Owner, onDelete, onZero bool) (bool, error) {
	if !(onDelete || onZero) || !owner.Valid() {
		return false, nil
	}
	obj, err := c.get(ctx, owner.Kind, owner.Namespace, owner.Name)
	if errors.Is(err, errNotFound) {
		return onDelete, nil
	}
	if err != nil {
		return false, err
	}
	if obj.Metadata.UID != owner.UID || (obj.Metadata.DeletionTimestamp != nil && !obj.Metadata.DeletionTimestamp.IsZero()) {
		return onDelete, nil
	}
	return onZero && obj.Spec.Replicas != nil && *obj.Spec.Replicas == 0, nil
}
