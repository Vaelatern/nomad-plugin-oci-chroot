package driver

import (
	"context"
	"fmt"
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

func buildahVersion() string {
	out, err := exec.Command("buildah", "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func buildahRunContext(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "buildah", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildah %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

func buildahOutputContext(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "buildah", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("buildah %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}
