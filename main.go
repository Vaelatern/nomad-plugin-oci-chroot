package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"

	"oci-chroot-driver/driver"
)

func main() {
	if os.Getenv("_OCI_CHROOT_EXEC") == "1" {
		chrootExec()
		return
	}

	plugins.Serve(func(logger hclog.Logger) interface{} {
		return driver.NewOCIDriver(logger)
	})
}

func ensureDir(path string, mode os.FileMode) {
	if err := os.MkdirAll(path, mode); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s failed: %v\n", path, err)
	}
}

func mkdev(major, minor int) int {
	return (((major) & 0xFFFFFFF) << 32) | ((minor) & 0xFFFFFFFF)
}

func mountTmpfs(path string, size string) {
	ensureDir(path, 0755)
	if err := syscall.Mount("tmpfs", path, "tmpfs", 0, "size="+size+",mode=0755"); err != nil {
		fmt.Fprintf(os.Stderr, "mount tmpfs %s failed: %v\n", path, err)
	}
}

func bindMount(src, dst string) {
	ensureDir(dst, 0755)
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
		fmt.Fprintf(os.Stderr, "bind mount %s -> %s failed: %v\n", src, dst, err)
	}
}

func chrootExec() {
	fmt.Fprintf(os.Stderr, "[oci-chroot] === chroot executor started ===\n")

	mountPoint := os.Getenv("_OCI_CHROOT_MOUNTPOINT")
	if mountPoint == "" {
		fmt.Fprintf(os.Stderr, "[oci-chroot] FATAL: _OCI_CHROOT_MOUNTPOINT not set — cannot chroot\n")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[oci-chroot] mount point: %s\n", mountPoint)

	// Verify mount point exists
	if _, err := os.Stat(mountPoint); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] FATAL: mount point %s does not exist: %v\n", mountPoint, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[oci-chroot] mount point verified\n")

	// Check what's in the mount point
	entries, _ := os.ReadDir(mountPoint)
	fmt.Fprintf(os.Stderr, "[oci-chroot] rootfs contents (%d entries):\n", len(entries))
	for i, e := range entries {
		if i >= 20 {
			fmt.Fprintf(os.Stderr, "[oci-chroot]   ... and %d more\n", len(entries)-i)
			break
		}
		fmt.Fprintf(os.Stderr, "[oci-chroot]   %s\n", e.Name())
	}

	// Check for /bin/sh in the chroot
	if _, err := os.Stat(mountPoint + "/bin/sh"); err == nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] /bin/sh exists in rootfs\n")
	}

	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] FATAL: failed to unshare mount namespace: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[oci-chroot] mount namespace unshared\n")

	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: failed to make / private: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[oci-chroot] root mount set to private\n")
	}

	mp := func(p string) string { return mountPoint + p }

	// Mount /proc
	fmt.Fprintf(os.Stderr, "[oci-chroot] mounting /proc\n")
	mountTmpfs(mp("/proc"), "0")
	if err := syscall.Mount("proc", mp("/proc"), "proc", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: mount /proc failed: %v\n", err)
	}

	// Mount /dev
	fmt.Fprintf(os.Stderr, "[oci-chroot] mounting /dev\n")
	mountTmpfs(mp("/dev"), "10M")
	if err := syscall.Mknod(mp("/dev/null"), syscall.S_IFCHR|0666, mkdev(1, 3)); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: mknod /dev/null failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/zero"), syscall.S_IFCHR|0666, mkdev(1, 5)); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: mknod /dev/zero failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/random"), syscall.S_IFCHR|0666, mkdev(1, 8)); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: mknod /dev/random failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/urandom"), syscall.S_IFCHR|0666, mkdev(1, 9)); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: mknod /dev/urandom failed: %v\n", err)
	}
	mountTmpfs(mp("/dev/pts"), "1M")
	if err := syscall.Mount("devpts", mp("/dev/pts"), "devpts", 0, "mode=0620,ptmxmode=0666"); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: mount /dev/pts failed: %v\n", err)
	}
	mountTmpfs(mp("/dev/shm"), "64M")
	if err := syscall.Mount("tmpfs", mp("/dev/shm"), "tmpfs", 0, "size=64M,mode=1777"); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: mount /dev/shm failed: %v\n", err)
	}

	// Mount /tmp
	fmt.Fprintf(os.Stderr, "[oci-chroot] mounting /tmp\n")
	mountTmpfs(mp("/tmp"), "100M")
	if err := syscall.Mount("tmpfs", mp("/tmp"), "tmpfs", 0, "size=100M,mode=1777"); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: mount /tmp failed: %v\n", err)
	}

	// Mount /sys
	fmt.Fprintf(os.Stderr, "[oci-chroot] mounting /sys\n")
	mountTmpfs(mp("/sys"), "0")
	if err := syscall.Mount("sysfs", mp("/sys"), "sysfs", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: mount /sys failed: %v\n", err)
	}

	// Mount /run (needed for Alpine /var/run -> ../run symlink and socket bind-mounts)
	fmt.Fprintf(os.Stderr, "[oci-chroot] mounting /run\n")
	mountTmpfs(mp("/run"), "10M")

	// Bind-mount task directories from host (NOMAD_ALLOC_DIR, NOMAD_TASK_DIR, NOMAD_SECRETS_DIR)
	dirsB64 := os.Getenv("_OCI_CHROOT_DIRS")
	if dirsB64 != "" {
		js, err := base64.StdEncoding.DecodeString(dirsB64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: failed to decode dirs: %v\n", err)
		} else {
			var dirs map[string]string
			if err := json.Unmarshal(js, &dirs); err != nil {
				fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: failed to parse dirs JSON: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "[oci-chroot] bind-mounting %d host directories:\n", len(dirs))
				for chrootPath, hostPath := range dirs {
					fmt.Fprintf(os.Stderr, "[oci-chroot]   %s <- %s", chrootPath, hostPath)
					if _, err := os.Stat(hostPath); err != nil {
						fmt.Fprintf(os.Stderr, " (HOST PATH NOT FOUND: %v)\n", err)
						continue
					}
					fmt.Fprintf(os.Stderr, "\n")
					dst := mp(chrootPath)
					os.RemoveAll(dst)
					ensureDir(dst, 0755)
					if err := syscall.Mount(hostPath, dst, "", syscall.MS_BIND, ""); err != nil {
						fmt.Fprintf(os.Stderr, "[oci-chroot]   ERROR: bind mount failed: %v\n", err)
					} else {
						fmt.Fprintf(os.Stderr, "[oci-chroot]   -> mounted at %s\n", dst)
					}
				}
				fmt.Fprintf(os.Stderr, "[oci-chroot] note: Nomad template stanzas render to /local and /secrets — these paths are now available inside the chroot\n")
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "[oci-chroot] no host directories to bind-mount\n")
	}

	// Bind-mount sockets from host
	socketsB64 := os.Getenv("_OCI_CHROOT_BIND_SOCKETS")
	if socketsB64 != "" {
		js, err := base64.StdEncoding.DecodeString(socketsB64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: failed to decode sockets: %v\n", err)
		} else {
			var sockets []string
			if err := json.Unmarshal(js, &sockets); err != nil {
				fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: failed to parse sockets JSON: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "[oci-chroot] bind-mounting %d host sockets:\n", len(sockets))
				for _, sockPath := range sockets {
					if _, err := os.Stat(sockPath); err != nil {
						fmt.Fprintf(os.Stderr, "[oci-chroot]   %s not found on host, skipping\n", sockPath)
						continue
					}
					dst := mp(sockPath)
					ensureDir(filepath.Dir(dst), 0755)
					os.WriteFile(dst, nil, 0644)
					if err := syscall.Mount(sockPath, dst, "", syscall.MS_BIND, ""); err != nil {
						fmt.Fprintf(os.Stderr, "[oci-chroot]   ERROR: bind mount socket %s -> %s failed: %v\n", sockPath, dst, err)
					} else {
						fmt.Fprintf(os.Stderr, "[oci-chroot]   socket mounted: %s -> %s\n", sockPath, dst)
					}
				}
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "[oci-chroot] no host sockets to bind-mount\n")
	}

	// Bind-mount host resolv.conf
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] bind-mounting /etc/resolv.conf\n")
		dst := mp("/etc/resolv.conf")
		os.WriteFile(dst, nil, 0644)
		if err := syscall.Mount("/etc/resolv.conf", dst, "", syscall.MS_BIND, ""); err != nil {
			fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: bind mount resolv.conf failed: %v\n", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "[oci-chroot] /etc/resolv.conf not found on host, skipping\n")
	}

	fmt.Fprintf(os.Stderr, "[oci-chroot] all mounts complete, performing chroot into %s\n", mountPoint)
	if err := syscall.Chroot(mountPoint); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] FATAL: chroot into %s failed: %v\n", mountPoint, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[oci-chroot] chroot successful, changing to /\n")
	if err := os.Chdir("/"); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] FATAL: chdir to / after chroot failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[oci-chroot] inside chroot, listing /\n")
	if entries, err := os.ReadDir("/"); err == nil {
		for _, e := range entries {
			fmt.Fprintf(os.Stderr, "[oci-chroot]   /%s\n", e.Name())
		}
	}

	// Change to WORKDIR if set
	workDir := os.Getenv("_OCI_CHROOT_WORKDIR")
	if workDir != "" {
		fmt.Fprintf(os.Stderr, "[oci-chroot] changing to workdir: %s\n", workDir)
		if _, err := os.Stat(workDir); err != nil {
			fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: workdir %s does not exist: %v\n", workDir, err)
		} else if err := os.Chdir(workDir); err != nil {
			fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: chdir to workdir %s failed: %v\n", workDir, err)
		} else {
			fmt.Fprintf(os.Stderr, "[oci-chroot] working directory is now: %s\n", workDir)
		}
	} else {
		fmt.Fprintf(os.Stderr, "[oci-chroot] no WORKDIR set, staying at /\n")
	}

	cmd := os.Getenv("_OCI_CHROOT_COMMAND")
	argsB64 := os.Getenv("_OCI_CHROOT_ARGS")
	var args []string
	if argsB64 != "" {
		js, err := base64.StdEncoding.DecodeString(argsB64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[oci-chroot] WARN: failed to decode args: %v\n", err)
		} else {
			json.Unmarshal(js, &args)
		}
	}
	args = append([]string{cmd}, args...)

	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "_OCI_CHROOT_") {
			if strings.HasPrefix(e, "PATH=") {
				path := strings.TrimPrefix(e, "PATH=")
				var dirs []string
				for _, d := range strings.Split(path, ":") {
					if d != "" {
						dirs = append(dirs, d)
					}
				}
				needBin, needUsrLocalBin := true, true
				for _, d := range dirs {
					if d == "/bin" {
						needBin = false
					}
					if d == "/usr/local/bin" {
						needUsrLocalBin = false
					}
				}
				if needUsrLocalBin {
					dirs = append([]string{"/usr/local/bin"}, dirs...)
				}
				if needBin {
					dirs = append(dirs, "/bin")
				}
				env = append(env, "PATH="+strings.Join(dirs, ":"))
			} else {
				env = append(env, e)
			}
		}
	}
	hasPath := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
			break
		}
	}
	if !hasPath {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	}

	fmt.Fprintf(os.Stderr, "[oci-chroot] executing task:\n")
	fmt.Fprintf(os.Stderr, "[oci-chroot]   command: %s\n", cmd)
	fmt.Fprintf(os.Stderr, "[oci-chroot]   args: %v\n", args[1:])
	fmt.Fprintf(os.Stderr, "[oci-chroot]   env vars: %d total\n", len(env))
	fmt.Fprintf(os.Stderr, "[oci-chroot] === chroot exec ===\n")

	if err := syscall.Exec(cmd, args, env); err != nil {
		fmt.Fprintf(os.Stderr, "[oci-chroot] FATAL: exec of %s failed: %v\n", cmd, err)
		os.Exit(1)
	}
}
