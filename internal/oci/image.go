package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	imageInspectTimeout = 5 * time.Second
	imageInspectLimit   = 1 << 20
)

var (
	fullCRIContainerID  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	imageDigest         = regexp.MustCompile(`^(?:sha256:[0-9a-f]{64}|sha384:[0-9a-f]{96}|sha512:[0-9a-f]{128})$`)
	imageRepository     = regexp.MustCompile(`^(?:(?:[a-zA-Z0-9.-]+|\[[0-9a-fA-F:]+\])(?::[0-9]+)?/)?[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	errImageOutputLimit = errors.New("crictl inspect output exceeds 1 MiB")
)

// ContainerImageID returns the immutable image reference from the K3s CRI
// status for a full containerd ID. The binary and endpoint are host settings.
// The reference is returned as reported: a raw digest or a repository digest.
func ContainerImageID(ctx context.Context, k3sBin, endpoint, id string) (string, error) {
	if !fullCRIContainerID.MatchString(id) {
		return "", errors.New("crictl inspect requires a full container ID")
	}
	if !filepath.IsAbs(k3sBin) {
		return "", errors.New("crictl inspect requires an absolute K3s binary path")
	}
	socket, unix := strings.CutPrefix(endpoint, "unix://")
	if !unix || !filepath.IsAbs(socket) || filepath.Clean(socket) == "/" {
		return "", errors.New("crictl inspect requires a local Unix socket endpoint")
	}

	ctx, cancel := context.WithTimeout(ctx, imageInspectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, k3sBin, "crictl", "--runtime-endpoint", endpoint,
		"inspect", "--quiet", "--output", "json", id)
	cmd.WaitDelay = 250 * time.Millisecond // Bound waits on inherited stdout pipes.
	var output imageInspectOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		if output.exceeded {
			return "", errImageOutputLimit
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", fmt.Errorf("crictl inspect: %w", err)
	}

	// Containerd fills ImageRef with a repo digest when one is available and
	// falls back to the stored image ID. Status.Image can be a mutable tag.
	var response struct {
		Status struct {
			ID       string `json:"id"`
			ImageRef string `json:"imageRef"`
		} `json:"status"`
	}
	if err := json.Unmarshal(output.data.Bytes(), &response); err != nil {
		return "", fmt.Errorf("decode crictl inspect status: %w", err)
	}
	if response.Status.ID != id {
		return "", errors.New("crictl inspect status does not match the requested container ID")
	}
	ref := response.Status.ImageRef
	repo, digest, hasRepo := strings.Cut(ref, "@")
	if !hasRepo {
		digest = ref
	}
	if !imageDigest.MatchString(digest) || (hasRepo && !imageRepository.MatchString(repo)) {
		return "", errors.New("crictl inspect status lacks an immutable imageRef")
	}
	return ref, nil
}

type imageInspectOutput struct {
	data     bytes.Buffer
	exceeded bool
}

func (out *imageInspectOutput) Write(p []byte) (int, error) {
	if len(p) > imageInspectLimit-out.data.Len() {
		out.exceeded = true
		return 0, errImageOutputLimit
	}
	return out.data.Write(p)
}
