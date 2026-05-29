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
	"golang.org/x/sys/unix"

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

func logf(quiet bool, format string, args ...interface{}) {
	if !quiet {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

func chrootExec() {
	quiet := os.Getenv("_OCI_CHROOT_QUIET") == "1"
	logf(quiet, "[oci-chroot] === chroot executor started ===\n")

	mountPoint := os.Getenv("_OCI_CHROOT_MOUNTPOINT")
	if mountPoint == "" {
		logf(quiet, "[oci-chroot] FATAL: _OCI_CHROOT_MOUNTPOINT not set — cannot chroot\n")
		os.Exit(1)
	}
	logf(quiet, "[oci-chroot] mount point: %s\n", mountPoint)

	// Verify mount point exists
	if _, err := os.Stat(mountPoint); err != nil {
		logf(quiet, "[oci-chroot] FATAL: mount point %s does not exist: %v\n", mountPoint, err)
		os.Exit(1)
	}
	logf(quiet, "[oci-chroot] mount point verified\n")

	// Enter network namespace if requested (bridge/group network mode)
	netnsPath := os.Getenv("_OCI_CHROOT_NETNS")
	if netnsPath != "" {
		logf(quiet, "[oci-chroot] joining network namespace: %s\n", netnsPath)
		f, err := os.Open(netnsPath)
		if err != nil {
			logf(quiet, "[oci-chroot] FATAL: cannot open netns %s: %v\n", netnsPath, err)
			os.Exit(1)
		}
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			f.Close()
			logf(quiet, "[oci-chroot] FATAL: setns(CLONE_NEWNET) failed for %s: %v\n", netnsPath, err)
			os.Exit(1)
		}
		f.Close()
		logf(quiet, "[oci-chroot] network namespace joined: %s\n", netnsPath)
	} else {
		logf(quiet, "[oci-chroot] no network namespace to join (host/group networking)\n")
	}

	// Check what's in the mount point
	entries, _ := os.ReadDir(mountPoint)
	logf(quiet, "[oci-chroot] rootfs contents (%d entries):\n", len(entries))
	for i, e := range entries {
		if i >= 20 {
			logf(quiet, "[oci-chroot]   ... and %d more\n", len(entries)-i)
			break
		}
		logf(quiet, "[oci-chroot]   %s\n", e.Name())
	}

	// Check for /bin/sh in the chroot
	if _, err := os.Stat(mountPoint + "/bin/sh"); err == nil {
		logf(quiet, "[oci-chroot] /bin/sh exists in rootfs\n")
	}

	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		logf(quiet, "[oci-chroot] FATAL: failed to unshare mount namespace: %v\n", err)
		os.Exit(1)
	}
	logf(quiet, "[oci-chroot] mount namespace unshared\n")

	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		logf(quiet, "[oci-chroot] WARN: failed to make / private: %v\n", err)
	} else {
		logf(quiet, "[oci-chroot] root mount set to private\n")
	}

	mp := func(p string) string { return mountPoint + p }

	// Mount /proc
	logf(quiet, "[oci-chroot] mounting /proc\n")
	mountTmpfs(mp("/proc"), "0")
	if err := syscall.Mount("proc", mp("/proc"), "proc", 0, ""); err != nil {
		logf(quiet, "[oci-chroot] WARN: mount /proc failed: %v\n", err)
	}

	// Mount /dev
	logf(quiet, "[oci-chroot] mounting /dev\n")
	mountTmpfs(mp("/dev"), "10M")
	mkdev := func(major, minor uint32) int {
		return int(unix.Mkdev(major, minor))
	}
	if err := syscall.Mknod(mp("/dev/null"), syscall.S_IFCHR|0666, mkdev(1, 3)); err != nil {
		logf(quiet, "[oci-chroot] WARN: mknod /dev/null failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/zero"), syscall.S_IFCHR|0666, mkdev(1, 5)); err != nil {
		logf(quiet, "[oci-chroot] WARN: mknod /dev/zero failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/random"), syscall.S_IFCHR|0666, mkdev(1, 8)); err != nil {
		logf(quiet, "[oci-chroot] WARN: mknod /dev/random failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/urandom"), syscall.S_IFCHR|0666, mkdev(1, 9)); err != nil {
		logf(quiet, "[oci-chroot] WARN: mknod /dev/urandom failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/full"), syscall.S_IFCHR|0666, mkdev(1, 7)); err != nil {
		logf(quiet, "[oci-chroot] WARN: mknod /dev/full failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/tty"), syscall.S_IFCHR|0666, mkdev(5, 0)); err != nil {
		logf(quiet, "[oci-chroot] WARN: mknod /dev/tty failed: %v\n", err)
	}
	mountTmpfs(mp("/dev/pts"), "1M")
	if err := syscall.Mount("devpts", mp("/dev/pts"), "devpts", 0, "mode=0620,ptmxmode=0666"); err != nil {
		logf(quiet, "[oci-chroot] WARN: mount /dev/pts failed: %v\n", err)
	}
	mountTmpfs(mp("/dev/shm"), "64M")
	if err := syscall.Mount("tmpfs", mp("/dev/shm"), "tmpfs", 0, "size=64M,mode=1777"); err != nil {
		logf(quiet, "[oci-chroot] WARN: mount /dev/shm failed: %v\n", err)
	}

	// Mount /tmp
	logf(quiet, "[oci-chroot] mounting /tmp\n")
	mountTmpfs(mp("/tmp"), "100M")
	if err := syscall.Mount("tmpfs", mp("/tmp"), "tmpfs", 0, "size=100M,mode=1777"); err != nil {
		logf(quiet, "[oci-chroot] WARN: mount /tmp failed: %v\n", err)
	}

	// Mount /sys
	logf(quiet, "[oci-chroot] mounting /sys\n")
	mountTmpfs(mp("/sys"), "0")
	if err := syscall.Mount("sysfs", mp("/sys"), "sysfs", 0, ""); err != nil {
		logf(quiet, "[oci-chroot] WARN: mount /sys failed: %v\n", err)
	}

	// Mount /run (needed for Alpine /var/run -> ../run symlink and socket bind-mounts)
	logf(quiet, "[oci-chroot] mounting /run\n")
	mountTmpfs(mp("/run"), "10M")

	// Bind-mount task directories from host (NOMAD_ALLOC_DIR, NOMAD_TASK_DIR, NOMAD_SECRETS_DIR)
	dirsB64 := os.Getenv("_OCI_CHROOT_DIRS")
	if dirsB64 != "" {
		js, err := base64.StdEncoding.DecodeString(dirsB64)
		if err != nil {
			logf(quiet, "[oci-chroot] WARN: failed to decode dirs: %v\n", err)
		} else {
			var dirs map[string]string
			if err := json.Unmarshal(js, &dirs); err != nil {
				logf(quiet, "[oci-chroot] WARN: failed to parse dirs JSON: %v\n", err)
			} else {
				logf(quiet, "[oci-chroot] bind-mounting %d host directories:\n", len(dirs))
				for chrootPath, hostPath := range dirs {
					logf(quiet, "[oci-chroot]   %s <- %s", chrootPath, hostPath)
					if _, err := os.Stat(hostPath); err != nil {
						logf(quiet, " (HOST PATH NOT FOUND: %v)\n", err)
						continue
					}
					logf(quiet, "\n")
					dst := mp(chrootPath)
					os.RemoveAll(dst)
					ensureDir(dst, 0755)
					if err := syscall.Mount(hostPath, dst, "", syscall.MS_BIND, ""); err != nil {
						logf(quiet, "[oci-chroot]   ERROR: bind mount failed: %v\n", err)
					} else {
						logf(quiet, "[oci-chroot]   -> mounted at %s\n", dst)
					}
				}
				logf(quiet, "[oci-chroot] note: Nomad template stanzas render to /local and /secrets — these paths are now available inside the chroot\n")
			}
		}
	} else {
		logf(quiet, "[oci-chroot] no host directories to bind-mount\n")
	}

	// Bind-mount sockets from host
	socketsB64 := os.Getenv("_OCI_CHROOT_BIND_SOCKETS")
	if socketsB64 != "" {
		js, err := base64.StdEncoding.DecodeString(socketsB64)
		if err != nil {
			logf(quiet, "[oci-chroot] WARN: failed to decode sockets: %v\n", err)
		} else {
			var sockets []string
			if err := json.Unmarshal(js, &sockets); err != nil {
				logf(quiet, "[oci-chroot] WARN: failed to parse sockets JSON: %v\n", err)
			} else {
				logf(quiet, "[oci-chroot] bind-mounting %d host sockets:\n", len(sockets))
				for _, sockPath := range sockets {
					if _, err := os.Stat(sockPath); err != nil {
						logf(quiet, "[oci-chroot]   %s not found on host, skipping\n", sockPath)
						continue
					}
					dst := mp(sockPath)
					ensureDir(filepath.Dir(dst), 0755)
					os.WriteFile(dst, nil, 0644)
					if err := syscall.Mount(sockPath, dst, "", syscall.MS_BIND, ""); err != nil {
						logf(quiet, "[oci-chroot]   ERROR: bind mount socket %s -> %s failed: %v\n", sockPath, dst, err)
					} else {
						logf(quiet, "[oci-chroot]   socket mounted: %s -> %s\n", sockPath, dst)
					}
				}
			}
		}
	} else {
		logf(quiet, "[oci-chroot] no host sockets to bind-mount\n")
	}

	// Copy host resolv.conf into chroot so DNS works inside the chroot
	if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		logf(quiet, "[oci-chroot] copying /etc/resolv.conf into chroot\n")
		dst := mp("/etc/resolv.conf")
		os.MkdirAll(filepath.Dir(dst), 0755)
		if err := os.WriteFile(dst, data, 0644); err != nil {
			logf(quiet, "[oci-chroot] WARN: failed to write resolv.conf into chroot: %v\n", err)
		}
	} else {
		logf(quiet, "[oci-chroot] /etc/resolv.conf not found on host, skipping\n")
	}

	logf(quiet, "[oci-chroot] all mounts complete, performing chroot into %s\n", mountPoint)
	if err := syscall.Chroot(mountPoint); err != nil {
		logf(quiet, "[oci-chroot] FATAL: chroot into %s failed: %v\n", mountPoint, err)
		os.Exit(1)
	}
	logf(quiet, "[oci-chroot] chroot successful, changing to /\n")
	if err := os.Chdir("/"); err != nil {
		logf(quiet, "[oci-chroot] FATAL: chdir to / after chroot failed: %v\n", err)
		os.Exit(1)
	}
	logf(quiet, "[oci-chroot] inside chroot, listing /\n")
	if entries, err := os.ReadDir("/"); err == nil {
		for _, e := range entries {
			logf(quiet, "[oci-chroot]   /%s\n", e.Name())
		}
	}

	// Change to WORKDIR if set
	workDir := os.Getenv("_OCI_CHROOT_WORKDIR")
	if workDir != "" {
		logf(quiet, "[oci-chroot] changing to workdir: %s\n", workDir)
		if _, err := os.Stat(workDir); err != nil {
			logf(quiet, "[oci-chroot] WARN: workdir %s does not exist: %v\n", workDir, err)
		} else if err := os.Chdir(workDir); err != nil {
			logf(quiet, "[oci-chroot] WARN: chdir to workdir %s failed: %v\n", workDir, err)
		} else {
			logf(quiet, "[oci-chroot] working directory is now: %s\n", workDir)
		}
	} else {
		logf(quiet, "[oci-chroot] no WORKDIR set, staying at /\n")
	}

	cmd := os.Getenv("_OCI_CHROOT_COMMAND")
	argsB64 := os.Getenv("_OCI_CHROOT_ARGS")
	var args []string
	if argsB64 != "" {
		js, err := base64.StdEncoding.DecodeString(argsB64)
		if err != nil {
			logf(quiet, "[oci-chroot] WARN: failed to decode args: %v\n", err)
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

	logf(quiet, "[oci-chroot] executing task:\n")
	logf(quiet, "[oci-chroot]   command: %s\n", cmd)
	logf(quiet, "[oci-chroot]   args: %v\n", args[1:])
	logf(quiet, "[oci-chroot]   env vars: %d total\n", len(env))
	logf(quiet, "[oci-chroot] === chroot exec ===\n")

	if err := syscall.Exec(cmd, args, env); err != nil {
		logf(quiet, "[oci-chroot] FATAL: exec of %s failed: %v\n", cmd, err)
		os.Exit(1)
	}
}
