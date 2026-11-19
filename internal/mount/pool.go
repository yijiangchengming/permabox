//go:build linux

package mount

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yijiangchengming/permabox/internal/workload"
)

const (
	deploymentMarker = ".permabox-deployment.json"
	deploymentPrefix = "deployment_" // Not a valid Kubernetes Pod name.
)

type DeploymentClaim struct {
	Pod, PodUID, Image, Runtime string
}

type deploymentSlot struct {
	DeploymentClaim
	DeploymentUID string
	Container     string
}

type deploymentCandidate struct {
	path     string
	claim    deploymentSlot
	released time.Time
	marker   os.FileInfo
}

// PrepareDeployment selects a node-local layer; the final selection and mount
// use the same lock as cleanup and GC. API checks never hold that lock.
func (m Manager) PrepareDeployment(ctx context.Context, id, root, storage, container string,
	owner workload.Owner, claim DeploymentClaim, ttl time.Duration,
	podFinished func(context.Context, string, string) (bool, error)) (bool, error) {
	if !owner.Valid() || owner.Kind != "Deployment" || claim.Pod == "" || claim.PodUID == "" ||
		container == "" || !filepath.IsAbs(claim.Runtime) || !filepath.IsAbs(storage) {
		return false, errors.New("incomplete deployment layer identity")
	}
	if err := os.MkdirAll(storage, 0700); err != nil {
		return false, err
	}
	storage, err := filepath.EvalSymlinks(storage)
	if err != nil {
		return false, err
	}
	parent := filepath.Join(storage, owner.Namespace)
	if err := os.Mkdir(parent, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return false, err
	}
	if err := realDir(parent); err != nil {
		return false, err
	}
	want := deploymentSlot{DeploymentClaim: claim, DeploymentUID: owner.UID, Container: container}
	fresh := filepath.Join(parent, fmt.Sprintf("%s%x", deploymentPrefix, sha256.Sum256([]byte(id))))
	var candidates []deploymentCandidate
	if claim.Image != "" {
		candidates, err = eligibleDeploymentSlots(ctx, parent, want, ttl, podFinished)
		if err != nil {
			return false, err
		}
	}
	return m.prepareOwned(id, root, fresh, container, owner, &want, func() (string, error) {
		if _, err := os.Lstat(fresh); err == nil || !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("fresh deployment layer is not available: %s", fresh)
		}
		if len(candidates) == 0 {
			return "", nil
		}
		files, err := os.ReadDir(m.Dir)
		if err != nil {
			return "", err
		}
		var live []record
		for _, file := range files {
			if strings.HasSuffix(file.Name(), ".json") {
				state, err := readRecord(filepath.Join(m.Dir, file.Name()))
				if err != nil {
					return "", err
				}
				live = append(live, state)
			}
		}
		mounts, err := m.sys.mounts()
		if err != nil {
			return "", err
		}
		for _, candidate := range candidates {
			current, ok := readDeploymentCandidate(candidate.path)
			if ok && current.claim == candidate.claim && current.released.Equal(candidate.released) &&
				os.SameFile(current.marker, candidate.marker) && !expiredDeploymentSlot(current, ttl) &&
				!journalReferences(live, candidate.path) && !mountReferences(mounts, candidate.path) {
				return candidate.path, nil
			}
		}
		return "", nil
	})
}

func eligibleDeploymentSlots(ctx context.Context, parent string, want deploymentSlot, ttl time.Duration,
	podFinished func(context.Context, string, string) (bool, error)) ([]deploymentCandidate, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	var same, peers []deploymentCandidate
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), deploymentPrefix) {
			continue
		}
		candidate, ok := readDeploymentCandidate(filepath.Join(parent, entry.Name()))
		if !ok || expiredDeploymentSlot(candidate, ttl) {
			continue
		}
		got := candidate.claim
		if got.DeploymentUID != want.DeploymentUID || got.Container != want.Container ||
			got.Runtime != want.Runtime || got.Image != want.Image || got.Pod == "" || got.PodUID == "" {
			continue
		}
		if got.PodUID == want.PodUID && got.Pod == want.Pod {
			same = append(same, candidate)
		} else if podFinished != nil && ctx.Err() == nil {
			if ok, err := podFinished(ctx, got.Pod, got.PodUID); err == nil && ok {
				peers = append(peers, candidate)
			}
		}
	}
	return append(same, peers...), nil
}

func expiredDeploymentSlot(candidate deploymentCandidate, ttl time.Duration) bool {
	return ttl > 0 && time.Since(candidate.released) >= ttl
}

func readDeploymentCandidate(path string) (deploymentCandidate, bool) {
	candidate := deploymentCandidate{path: path}
	owned, err := ownedSlot(path)
	if err != nil || !owned || !readMarker(filepath.Join(path, deploymentMarker), &candidate.claim) {
		return candidate, false
	}
	marker := filepath.Join(path, releasedMarker)
	info, err := os.Lstat(marker)
	if err != nil || !readMarker(marker, &candidate.released) || candidate.released.IsZero() {
		return candidate, false
	}
	candidate.marker = info
	return candidate, true
}
