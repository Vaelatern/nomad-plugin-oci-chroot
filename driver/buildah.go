package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const buildahTimeout = 5 * time.Minute

type cliBuildahBackend struct{}

func (b *cliBuildahBackend) Name() string { return "cli" }

func (b *cliBuildahBackend) Version() (string, error) {
	return buildahVersion(), nil
}

func (b *cliBuildahBackend) Available() (bool, string) {
	if _, err := exec.LookPath("buildah"); err != nil {
		return false, "buildah not found in PATH"
	}
	return true, ""
}

func (b *cliBuildahBackend) Pull(ctx context.Context, image string, force bool) error {
	if force {
		return buildahRunContext(ctx, "pull", "--policy=always", image)
	}
	return buildahRunContext(ctx, "pull", image)
}

func (b *cliBuildahBackend) From(ctx context.Context, image string) (string, error) {
	return buildahOutputContext(ctx, "from", image)
}

func (b *cliBuildahBackend) Mount(ctx context.Context, containerID string) (string, error) {
	return buildahOutputContext(ctx, "mount", containerID)
}

func (b *cliBuildahBackend) Unmount(ctx context.Context, containerID string) error {
	return buildahRunContext(ctx, "unmount", containerID)
}

func (b *cliBuildahBackend) Remove(ctx context.Context, containerID string) error {
	return buildahRunContext(ctx, "rm", containerID)
}

func (b *cliBuildahBackend) Inspect(ctx context.Context, image string) (*ImageConfig, error) {
	out, err := buildahOutputContext(ctx, "inspect", "--type", "image", image)
	if err != nil {
		return nil, err
	}
	var result struct {
		OCIv1 struct {
			Config struct {
				Entrypoint []string `json:"Entrypoint"`
				Cmd        []string `json:"Cmd"`
				WorkDir    string   `json:"WorkingDir"`
			} `json:"config"`
		} `json:"OCIv1"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("parse buildah inspect output: %w", err)
	}
	return &ImageConfig{
		Entrypoint: result.OCIv1.Config.Entrypoint,
		Cmd:        result.OCIv1.Config.Cmd,
		WorkDir:    result.OCIv1.Config.WorkDir,
	}, nil
}

func buildahVersion() string {
	out, err := exec.Command("buildah", "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func buildahRunContext(ctx context.Context, args ...string) error {
	cmdStr := "buildah " + strings.Join(args, " ")
	cmd := exec.CommandContext(ctx, "buildah", args...)
	fmt.Fprintf(os.Stderr, "[oci-chroot] running: %s\n", cmdStr)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] buildah command failed:\n  command: %s\n  error: %v\n  output: %s\n", cmdStr, err, string(out))
		return fmt.Errorf("buildah %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	if len(out) > 0 {
		fmt.Fprintf(os.Stderr, "[oci-chroot] buildah output: %s\n", strings.TrimSpace(string(out)))
	}
	return nil
}

func buildahOutputContext(ctx context.Context, args ...string) (string, error) {
	cmdStr := "buildah " + strings.Join(args, " ")
	cmd := exec.CommandContext(ctx, "buildah", args...)
	fmt.Fprintf(os.Stderr, "[oci-chroot] running: %s\n", cmdStr)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] buildah command failed:\n  command: %s\n  error: %v\n  output: %s\n", cmdStr, err, string(out))
		return "", fmt.Errorf("buildah %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	result := strings.TrimSpace(string(out))
	if len(result) > 0 {
		fmt.Fprintf(os.Stderr, "[oci-chroot] buildah output: %s\n", result)
	}
	return result, nil
}
