package workload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

type Owner struct {
	Namespace string    `json:"namespace"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	UID       string    `json:"uid"`
	Overrides Overrides `json:"gc_overrides,omitzero"`
}

const (
	deleteAnnotation      = "permabox.io/gc-on-workload-delete"
	scaleToZeroAnnotation = "permabox.io/gc-on-scale-to-zero"
	reusePolicyAnnotation = "permabox.io/reuse-policy"
)

type Overrides struct {
	Delete string `json:"delete,omitempty"`
	Zero   string `json:"zero,omitempty"`
}

func readOverrides(annotations map[string]string) Overrides {
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
	return Overrides{Delete: read(deleteAnnotation), Zero: read(scaleToZeroAnnotation)}
}

func (o Overrides) Apply(onDelete, onZero bool) (bool, bool) {
	if o.Delete != "" {
		onDelete = o.Delete == "true"
	}
	if o.Zero != "" {
		onZero = o.Zero == "true"
	}
	return onDelete, onZero
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func (o Owner) Valid() bool {
	return namePattern.MatchString(o.Namespace) && namePattern.MatchString(o.Name) && o.UID != "" &&
		(o.Kind == "Deployment" || o.Kind == "StatefulSet")
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
	Status *struct {
		Phase string `json:"phase,omitempty"`
	} `json:"status,omitempty"`
}

type Client struct {
	base string
	http *http.Client
	// Read the projected token per request so rotation needs no restart.
	tokenPath string
}

var errNotFound = errors.New("Kubernetes object not found")

var collection = map[string]string{
	"Pod": "pods", "ReplicaSet": "replicasets", "Deployment": "deployments", "StatefulSet": "statefulsets",
}

func resourcePath(kind, namespace, name string) (string, error) {
	resource, ok := collection[kind]
	if !ok || !namePattern.MatchString(namespace) || !namePattern.MatchString(name) {
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
			if owner.Controller || ref.APIVersion != "apps/v1" || ref.UID == "" || !namePattern.MatchString(ref.Name) {
				return reference{}
			}
			owner = ref
		}
	}
	return owner
}

func (c Client) Resolve(ctx context.Context, namespace, pod, uid string) (Owner, error) {
	if uid == "" {
		return Owner{}, nil
	}
	obj, err := c.get(ctx, "Pod", namespace, pod)
	if errors.Is(err, errNotFound) {
		return Owner{}, nil
	}
	if err != nil || obj.Metadata.UID != uid {
		return Owner{}, err
	}
	overrides := readOverrides(obj.Metadata.Annotations)
	ownedBy := func(ref reference) Owner {
		return Owner{Namespace: namespace, Kind: ref.Kind, Name: ref.Name, UID: ref.UID, Overrides: overrides}
	}
	ref := controller(obj)
	if ref.Kind == "StatefulSet" {
		return ownedBy(ref), nil
	}
	if ref.Kind == "ReplicaSet" {
		obj, err := c.get(ctx, ref.Kind, namespace, ref.Name)
		if errors.Is(err, errNotFound) {
			return Owner{}, nil
		}
		if err != nil || obj.Metadata.UID != ref.UID {
			return Owner{}, err
		}
		if ref = controller(obj); ref.Kind == "Deployment" {
			return ownedBy(ref), nil
		}
	}
	return Owner{}, nil
}

// ResolveDeployment returns a verified, non-deleting Deployment for a Pod
// that opted into deployment-local reuse. A zero Owner means no eligible match.
func (c Client) ResolveDeployment(ctx context.Context, namespace, pod, uid string) (Owner, error) {
	if uid == "" {
		return Owner{}, nil
	}
	active := func(kind, name, uid string) (object, bool, error) {
		obj, err := c.get(ctx, kind, namespace, name)
		if errors.Is(err, errNotFound) {
			return obj, false, nil
		}
		return obj, err == nil && obj.Metadata.UID == uid && obj.Metadata.DeletionTimestamp == nil, err
	}
	podObj, ok, err := active("Pod", pod, uid)
	if !ok || podObj.Metadata.Annotations[reusePolicyAnnotation] != "deployment-local" {
		return Owner{}, err
	}
	rsRef := controller(podObj)
	if rsRef.Kind != "ReplicaSet" {
		return Owner{}, nil
	}
	rs, ok, err := active(rsRef.Kind, rsRef.Name, rsRef.UID)
	if !ok {
		return Owner{}, err
	}
	depRef := controller(rs)
	if depRef.Kind != "Deployment" {
		return Owner{}, nil
	}
	if _, ok, err := active(depRef.Kind, depRef.Name, depRef.UID); !ok {
		return Owner{}, err
	}
	return Owner{Namespace: namespace, Kind: depRef.Kind, Name: depRef.Name, UID: depRef.UID,
		Overrides: readOverrides(podObj.Metadata.Annotations)}, nil
}

// PodFinished checks whether the API confirms that this Pod UID is gone or terminal.
func (c Client) PodFinished(ctx context.Context, namespace, name, uid string) (bool, error) {
	if uid == "" {
		return false, nil
	}
	obj, err := c.get(ctx, "Pod", namespace, name)
	if errors.Is(err, errNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return obj.Metadata.UID != uid || (obj.Status != nil && (obj.Status.Phase == "Succeeded" || obj.Status.Phase == "Failed")), nil
}

func (c Client) Reclaim(ctx context.Context, owner Owner, onDelete, onZero bool) (bool, error) {
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
