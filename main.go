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
	mountPoint := os.Getenv("_OCI_CHROOT_MOUNTPOINT")
	if mountPoint == "" {
		fmt.Fprintf(os.Stderr, "_OCI_CHROOT_MOUNTPOINT not set\n")
		os.Exit(1)
	}

	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		fmt.Fprintf(os.Stderr, "failed to unshare mount namespace: %v\n", err)
		os.Exit(1)
	}

	syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, "")

	mp := func(p string) string { return mountPoint + p }

	// Mount /proc
	mountTmpfs(mp("/proc"), "0")
	syscall.Mount("proc", mp("/proc"), "proc", 0, "")

	// Mount /dev
	mountTmpfs(mp("/dev"), "10M")
	if err := syscall.Mknod(mp("/dev/null"), syscall.S_IFCHR|0666, mkdev(1, 3)); err != nil {
		fmt.Fprintf(os.Stderr, "mknod /dev/null failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/zero"), syscall.S_IFCHR|0666, mkdev(1, 5)); err != nil {
		fmt.Fprintf(os.Stderr, "mknod /dev/zero failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/random"), syscall.S_IFCHR|0666, mkdev(1, 8)); err != nil {
		fmt.Fprintf(os.Stderr, "mknod /dev/random failed: %v\n", err)
	}
	if err := syscall.Mknod(mp("/dev/urandom"), syscall.S_IFCHR|0666, mkdev(1, 9)); err != nil {
		fmt.Fprintf(os.Stderr, "mknod /dev/urandom failed: %v\n", err)
	}
	mountTmpfs(mp("/dev/pts"), "1M")
	syscall.Mount("devpts", mp("/dev/pts"), "devpts", 0, "mode=0620,ptmxmode=0666")
	mountTmpfs(mp("/dev/shm"), "64M")
	syscall.Mount("tmpfs", mp("/dev/shm"), "tmpfs", 0, "size=64M,mode=1777")

	// Mount /tmp
	mountTmpfs(mp("/tmp"), "100M")
	syscall.Mount("tmpfs", mp("/tmp"), "tmpfs", 0, "size=100M,mode=1777")

	// Mount /sys
	mountTmpfs(mp("/sys"), "0")
	syscall.Mount("sysfs", mp("/sys"), "sysfs", 0, "")

	// Mount /run (needed for Alpine /var/run -> ../run symlink and socket bind-mounts)
	mountTmpfs(mp("/run"), "10M")

	// Bind-mount task directories from host (NOMAD_ALLOC_DIR, NOMAD_TASK_DIR, NOMAD_SECRETS_DIR)
	dirsB64 := os.Getenv("_OCI_CHROOT_DIRS")
	if dirsB64 != "" {
		js, _ := base64.StdEncoding.DecodeString(dirsB64)
		var dirs map[string]string
		if err := json.Unmarshal(js, &dirs); err == nil {
			for chrootPath, hostPath := range dirs {
				if _, err := os.Stat(hostPath); err == nil {
					dst := mp(chrootPath)
					os.RemoveAll(dst)
					ensureDir(dst, 0755)
					if err := syscall.Mount(hostPath, dst, "", syscall.MS_BIND, ""); err != nil {
						fmt.Fprintf(os.Stderr, "bind mount %s -> %s failed: %v\n", hostPath, dst, err)
					}
				}
			}
		}
	}

	// Bind-mount sockets from host
	socketsB64 := os.Getenv("_OCI_CHROOT_BIND_SOCKETS")
	if socketsB64 != "" {
		js, _ := base64.StdEncoding.DecodeString(socketsB64)
		var sockets []string
		if err := json.Unmarshal(js, &sockets); err == nil {
			for _, sockPath := range sockets {
				if _, err := os.Stat(sockPath); err != nil {
					fmt.Fprintf(os.Stderr, "socket %s not found on host, skipping\n", sockPath)
					continue
				}
				dst := mp(sockPath)
				ensureDir(filepath.Dir(dst), 0755)
				os.WriteFile(dst, nil, 0644)
				if err := syscall.Mount(sockPath, dst, "", syscall.MS_BIND, ""); err != nil {
					fmt.Fprintf(os.Stderr, "bind mount socket %s -> %s failed: %v\n", sockPath, dst, err)
				}
			}
		}
	}

	// Bind-mount host resolv.conf
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		dst := mp("/etc/resolv.conf")
		os.WriteFile(dst, nil, 0644)
		if err := syscall.Mount("/etc/resolv.conf", dst, "", syscall.MS_BIND, ""); err != nil {
			fmt.Fprintf(os.Stderr, "bind mount resolv.conf failed: %v\n", err)
		}
	}

	if err := syscall.Chroot(mountPoint); err != nil {
		fmt.Fprintf(os.Stderr, "failed to chroot: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chdir("/"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir: %v\n", err)
		os.Exit(1)
	}

	cmd := os.Getenv("_OCI_CHROOT_COMMAND")
	argsB64 := os.Getenv("_OCI_CHROOT_ARGS")
	var args []string
	if argsB64 != "" {
		js, _ := base64.StdEncoding.DecodeString(argsB64)
		json.Unmarshal(js, &args)
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

	if err := syscall.Exec(cmd, args, env); err != nil {
		fmt.Fprintf(os.Stderr, "failed to exec %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
