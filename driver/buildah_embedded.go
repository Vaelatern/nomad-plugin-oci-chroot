package driver

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type embeddedBuildahBackend struct {
	mu         sync.Mutex
	roots      map[string]string
	storageDir string
}

func newEmbeddedBuildahBackend() (*embeddedBuildahBackend, error) {
	storageDir := filepath.Join(os.TempDir(), "oci-chroot-store")
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	return &embeddedBuildahBackend{
		roots:      make(map[string]string),
		storageDir: storageDir,
	}, nil
}

func (b *embeddedBuildahBackend) Name() string { return "embedded" }

func (b *embeddedBuildahBackend) Version() (string, error) {
	return "go-containerregistry", nil
}

func (b *embeddedBuildahBackend) Available() (bool, string) {
	return true, ""
}

func (b *embeddedBuildahBackend) Pull(ctx context.Context, image string, force bool) error {
	ref, err := name.ParseReference(image)
	if err != nil {
		return fmt.Errorf("parse reference %s: %w", image, err)
	}

	// Verify the image is accessible by checking its manifest
	desc, err := remote.Get(ref)
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	_ = desc
	return nil
}

func (b *embeddedBuildahBackend) From(ctx context.Context, image string) (string, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", fmt.Errorf("parse reference %s: %w", image, err)
	}

	img, err := remote.Image(ref)
	if err != nil {
		return "", fmt.Errorf("remote image %s: %w", image, err)
	}

	// Get image digest for a unique container ID
	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("get digest: %w", err)
	}
	containerID := digest.Hex[:16]

	rootfs := filepath.Join(b.storageDir, containerID)

	// Skip extraction if already exists
	if _, err := os.Stat(rootfs); err == nil {
		b.mu.Lock()
		b.roots[containerID] = rootfs
		b.mu.Unlock()
		return containerID, nil
	}

	if err := os.MkdirAll(rootfs, 0755); err != nil {
		return "", fmt.Errorf("mkdir rootfs: %w", err)
	}

	// Extract image layers to rootfs directory
	rc := mutate.Extract(img)
	if err := extractTarStream(rc, rootfs); err != nil {
		os.RemoveAll(rootfs)
		return "", fmt.Errorf("extract image: %w", err)
	}

	b.mu.Lock()
	b.roots[containerID] = rootfs
	b.mu.Unlock()

	return containerID, nil
}

func (b *embeddedBuildahBackend) Mount(ctx context.Context, containerID string) (string, error) {
	b.mu.Lock()
	rootfs, ok := b.roots[containerID]
	b.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("container %s not found", containerID)
	}
	return rootfs, nil
}

func (b *embeddedBuildahBackend) Unmount(ctx context.Context, containerID string) error {
	return nil
}

func (b *embeddedBuildahBackend) Remove(ctx context.Context, containerID string) error {
	b.mu.Lock()
	rootfs, ok := b.roots[containerID]
	delete(b.roots, containerID)
	b.mu.Unlock()

	if ok && rootfs != "" {
		return os.RemoveAll(rootfs)
	}
	return nil
}

func extractTarStream(rc io.ReadCloser, root string) error {
	defer rc.Close()

	// Ensure the root directory exists
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}

	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}

		// Sanitize path to prevent zip-slip
		name := filepath.Clean(header.Name)
		if name == "." || name == "/" {
			continue
		}
		// Remove leading slash
		name = strings.TrimPrefix(name, "/")
		target := filepath.Join(root, name)

		// Ensure target is within root
		if !strings.HasPrefix(target, filepath.Clean(root)+string(os.PathSeparator)) && target != filepath.Clean(root) {
			continue // skip paths outside root
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o777); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}

		case tar.TypeReg, tar.TypeRegA:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			f.Close()

		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent for symlink %s: %w", target, err)
			}
			os.Remove(target) // Remove existing if any
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", target, header.Linkname, err)
			}

		case tar.TypeLink:
			linkTarget := filepath.Join(root, header.Linkname)
			// Ensure linkTarget is within root
			if !strings.HasPrefix(linkTarget, filepath.Clean(root)+string(os.PathSeparator)) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent for hardlink %s: %w", target, err)
			}
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", target, header.Linkname, err)
			}

		case tar.TypeChar:
			// Skip device files when not running as root
		case tar.TypeBlock:
			// Skip device files when not running as root
		case tar.TypeFifo:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent for fifo %s: %w", target, err)
			}
		}
	}

	return nil
}


