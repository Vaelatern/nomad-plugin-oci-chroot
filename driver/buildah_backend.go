package driver

import "context"

type BuildahBackend interface {
	Name() string
	Version() (string, error)
	Available() (bool, string)
	Pull(ctx context.Context, image string, force bool) error
	From(ctx context.Context, image string) (string, error)
	Mount(ctx context.Context, containerID string) (string, error)
	Unmount(ctx context.Context, containerID string) error
	Remove(ctx context.Context, containerID string) error
}
