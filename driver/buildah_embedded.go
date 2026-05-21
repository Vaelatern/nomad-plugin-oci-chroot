package driver

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// errLog is a logger that writes to stderr so messages from the embedded
// backend appear in Nomad client logs.
var errLog = log.New(os.Stderr, "[oci-chroot] ", log.LstdFlags|log.Lmsgprefix)

type embeddedBuildahBackend struct {
	mu         sync.Mutex
	roots      map[string]string
	storageDir string
}

func newEmbeddedBuildahBackend() (*embeddedBuildahBackend, error) {
	storageDir := filepath.Join(os.TempDir(), "oci-chroot-store")
	errLog.Printf("initializing embedded backend, storage dir: %s", storageDir)
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		errLog.Printf("failed to create storage directory: %v", err)
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	errLog.Printf("storage directory ready: %s", storageDir)
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
	errLog.Printf("pull: parsing image reference: %s", image)
	ref, err := name.ParseReference(image)
	if err != nil {
		errLog.Printf("pull: failed to parse image reference %s: %v", image, err)
		return fmt.Errorf("invalid image reference %q: %w", image, err)
	}
	errLog.Printf("pull: reference parsed: %s", ref.String())

	// Verify the image is accessible by checking its manifest
	errLog.Printf("pull: fetching manifest descriptor for %s", image)
	desc, err := remote.Get(ref)
	if err != nil {
		errLog.Printf("pull: failed to fetch %s: %v", image, err)
		return fmt.Errorf("pull image %q from %s: %w", image, ref.Context().RegistryStr(), err)
	}
	errLog.Printf("pull: image accessible — digest: %s, media type: %s", desc.Digest.String(), desc.MediaType)
	return nil
}

func (b *embeddedBuildahBackend) From(ctx context.Context, image string) (string, error) {
	errLog.Printf("from: parsing image reference: %s", image)
	ref, err := name.ParseReference(image)
	if err != nil {
		errLog.Printf("from: failed to parse image reference %s: %v", image, err)
		return "", fmt.Errorf("parse reference %s: %w", image, err)
	}
	errLog.Printf("from: resolved reference: %s", ref.String())

	errLog.Printf("from: fetching image metadata for %s", image)
	img, err := remote.Image(ref)
	if err != nil {
		errLog.Printf("from: failed to fetch image %s: %v", image, err)
		return "", fmt.Errorf("remote image %s: %w", image, err)
	}
	errLog.Printf("from: image metadata fetched")

	// Get image digest for a unique container ID
	digest, err := img.Digest()
	if err != nil {
		errLog.Printf("from: failed to get image digest: %v", err)
		return "", fmt.Errorf("get digest: %w", err)
	}
	containerID := digest.Hex[:16]
	errLog.Printf("from: image digest computed, container ID: %s", containerID)

	rootfs := filepath.Join(b.storageDir, containerID)
	errLog.Printf("from: rootfs path: %s", rootfs)

	// Skip extraction if already exists
	if _, err := os.Stat(rootfs); err == nil {
		errLog.Printf("from: rootfs already extracted, reusing cached copy at %s", rootfs)
		b.mu.Lock()
		b.roots[containerID] = rootfs
		b.mu.Unlock()
		return containerID, nil
	}
	errLog.Printf("from: rootfs not cached, extracting layers...")

	if err := os.MkdirAll(rootfs, 0755); err != nil {
		errLog.Printf("from: failed to create rootfs directory %s: %v", rootfs, err)
		return "", fmt.Errorf("mkdir rootfs: %w", err)
	}

	// Extract image layers to rootfs directory
	errLog.Printf("from: starting layer extraction to %s", rootfs)
	rc := mutate.Extract(img)
	if err := extractTarStream(rc, rootfs); err != nil {
		errLog.Printf("from: layer extraction failed: %v", err)
		os.RemoveAll(rootfs)
		return "", fmt.Errorf("extract image: %w", err)
	}
	errLog.Printf("from: layer extraction complete")

	b.mu.Lock()
	b.roots[containerID] = rootfs
	b.mu.Unlock()

	errLog.Printf("from: container ready — id=%s rootfs=%s", containerID, rootfs)
	return containerID, nil
}

func (b *embeddedBuildahBackend) Inspect(ctx context.Context, image string) (*ImageConfig, error) {
	errLog.Printf("inspect: fetching config for image %s", image)
	ref, err := name.ParseReference(image)
	if err != nil {
		errLog.Printf("inspect: failed to parse reference %s: %v", image, err)
		return nil, fmt.Errorf("parse reference %s: %w", image, err)
	}
	img, err := remote.Image(ref)
	if err != nil {
		errLog.Printf("inspect: failed to fetch image %s: %v", image, err)
		return nil, fmt.Errorf("remote image %s: %w", image, err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		errLog.Printf("inspect: failed to read config file for %s: %v", image, err)
		return nil, fmt.Errorf("config file %s: %w", image, err)
	}
	errLog.Printf("inspect: entrypoint=%v cmd=%v work_dir=%s", cf.Config.Entrypoint, cf.Config.Cmd, cf.Config.WorkingDir)
	return &ImageConfig{
		Entrypoint: cf.Config.Entrypoint,
		Cmd:        cf.Config.Cmd,
		WorkDir:    cf.Config.WorkingDir,
	}, nil
}

func (b *embeddedBuildahBackend) Mount(ctx context.Context, containerID string) (string, error) {
	errLog.Printf("mount: looking up container %s", containerID)
	b.mu.Lock()
	rootfs, ok := b.roots[containerID]
	b.mu.Unlock()
	if !ok {
		errLog.Printf("mount: container %s not found in local store", containerID)
		return "", fmt.Errorf("container %s not found", containerID)
	}
	errLog.Printf("mount: container %s -> %s", containerID, rootfs)
	return rootfs, nil
}

func (b *embeddedBuildahBackend) Unmount(ctx context.Context, containerID string) error {
	errLog.Printf("unmount: (no-op for embedded backend) container %s", containerID)
	return nil
}

func (b *embeddedBuildahBackend) Remove(ctx context.Context, containerID string) error {
	errLog.Printf("remove: cleaning up container %s", containerID)
	b.mu.Lock()
	rootfs, ok := b.roots[containerID]
	delete(b.roots, containerID)
	b.mu.Unlock()

	if ok && rootfs != "" {
		errLog.Printf("remove: deleting rootfs at %s", rootfs)
		if err := os.RemoveAll(rootfs); err != nil {
			errLog.Printf("remove: failed to delete rootfs %s: %v", rootfs, err)
			return err
		}
		errLog.Printf("remove: rootfs deleted")
	} else {
		errLog.Printf("remove: no rootfs path found for container %s", containerID)
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


