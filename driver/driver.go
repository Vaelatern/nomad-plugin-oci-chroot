package driver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/hashicorp/go-hclog"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
	"golang.org/x/sys/unix"
)

const pluginVersion = "0.1.0"

func signalFromString(name string) (os.Signal, error) {
	if name == "" {
		return syscall.SIGTERM, nil
	}
	// Accept both "SIGTERM" and "TERM" forms
	sigName := name
	if !strings.HasPrefix(name, "SIG") {
		sigName = "SIG" + name
	}
	if s := unix.SignalNum(sigName); s != 0 {
		return s, nil
	}
	// Try numeric
	if n, err := strconv.Atoi(name); err == nil {
		return syscall.Signal(n), nil
	}
	return nil, fmt.Errorf("unknown or unsupported signal: %q", name)
}

type OCIDriver struct {
	pluginName string
	backend    BuildahBackend
	logger     hclog.Logger

	config     *Config
	configLock sync.Mutex
	tasksLock  sync.Mutex
	tasks      map[string]*taskHandle
	taskEvents chan *drivers.TaskEvent
	ctx        context.Context
	cancel     context.CancelFunc
}

type Config struct {
	Enabled     bool `codec:"enabled"`
	HostBuildah bool `codec:"host_buildah"`
}

func NewOCIDriver(logger hclog.Logger) *OCIDriver {
	ctx, cancel := context.WithCancel(context.Background())
	return &OCIDriver{
		pluginName: "oci-chroot",
		logger:     logger.Named("oci-chroot"),
		tasks:      make(map[string]*taskHandle),
		taskEvents: make(chan *drivers.TaskEvent, 100),
		ctx:        ctx,
		cancel:     cancel,
	}
}

func (d *OCIDriver) PluginInfo() (*base.PluginInfoResponse, error) {
	return &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{"0.1.0"},
		PluginVersion:     pluginVersion,
		Name:              d.pluginName,
	}, nil
}

func (d *OCIDriver) ConfigSchema() (*hclspec.Spec, error) {
	return hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled":      hclspec.NewAttr("enabled", "bool", false),
		"host_buildah": hclspec.NewAttr("host_buildah", "bool", false),
	}), nil
}

func (d *OCIDriver) SetConfig(c *base.Config) error {
	d.configLock.Lock()
	defer d.configLock.Unlock()

	d.logger.Debug("SetConfig called", "plugin_config_bytes", len(c.PluginConfig))

	var config Config
	if len(c.PluginConfig) > 0 {
		if err := base.MsgPackDecode(c.PluginConfig, &config); err != nil {
			d.logger.Error("failed to decode plugin config", "error", err)
			return err
		}
	}
	d.config = &config

	d.logger.Info("plugin config loaded",
		"enabled", config.Enabled,
		"host_buildah", config.HostBuildah,
	)

	// Initialize buildah backend based on config
	if d.config.HostBuildah {
		d.logger.Info("using host buildah binary for image operations")
		d.backend = &cliBuildahBackend{}
		// Check if buildah is actually available
		if ok, reason := d.backend.Available(); !ok {
			d.logger.Warn("buildah binary not found but host_buildah=true", "reason", reason)
		} else {
			ver, _ := d.backend.Version()
			d.logger.Info("host buildah detected", "version", ver)
		}
	} else {
		d.logger.Info("using embedded buildah backend (go-containerregistry) — no external dependencies")
		backend, err := newEmbeddedBuildahBackend()
		if err != nil {
			d.logger.Error("failed to create embedded buildah backend", "error", err)
			return fmt.Errorf("failed to create embedded buildah backend: %v", err)
		}
		d.backend = backend
		ver, _ := d.backend.Version()
		d.logger.Debug("embedded backend initialized", "version", ver)
	}

	if d.config.Enabled {
		d.logger.Info("oci-chroot driver is ENABLED")
	} else {
		d.logger.Warn("oci-chroot driver is DISABLED — tasks using this driver will fail")
	}

	return nil
}

func (d *OCIDriver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (d *OCIDriver) Capabilities() (*drivers.Capabilities, error) {
	return &drivers.Capabilities{
		SendSignals: true,
		Exec:        true,
		FSIsolation: fsisolation.Chroot,
		NetIsolationModes: []drivers.NetIsolationMode{
			drivers.NetIsolationModeNone,
			drivers.NetIsolationModeHost,
			drivers.NetIsolationModeGroup,
			drivers.NetIsolationModeTask,
		},
		MountConfigs: drivers.MountConfigSupportNone,
	}, nil
}

func (d *OCIDriver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint, 1)

	go func() {
		defer close(ch)

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		var lastHealth drivers.HealthState

		for {
			health := drivers.HealthStateHealthy
			desc := drivers.DriverHealthy
			attrs := map[string]*pstructs.Attribute{}

			if d.backend != nil {
				buildahVer, _ := d.backend.Version()
				attrKey := "driver." + d.pluginName
				attrs = map[string]*pstructs.Attribute{
					attrKey:                      pstructs.NewStringAttribute("1"),
					attrKey + ".version":         pstructs.NewStringAttribute(pluginVersion),
					attrKey + ".buildah":         pstructs.NewStringAttribute(buildahVer),
					attrKey + ".buildah_backend": pstructs.NewStringAttribute(d.backend.Name()),
				}

				if ok, failDesc := d.backend.Available(); !ok {
					health = drivers.HealthStateUndetected
					desc = failDesc
				}
			} else {
				health = drivers.HealthStateUndetected
				desc = "backend not initialized (SetConfig not called)"
			}

			if health != lastHealth {
				d.logger.Info("fingerprint health changed",
					"from", lastHealth,
					"to", health,
					"description", desc,
				)
				lastHealth = health
			}

			select {
			case ch <- &drivers.Fingerprint{
				Attributes:        attrs,
				Health:            health,
				HealthDescription: desc,
			}:
			case <-ctx.Done():
				return
			}

			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

func (d *OCIDriver) RecoverTask(handle *drivers.TaskHandle) error {
	return d.recoverTask(handle)
}

func (d *OCIDriver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if d.backend == nil {
		return nil, nil, fmt.Errorf("buildah backend not initialized (SetConfig not called)")
	}

	d.tasksLock.Lock()
	defer d.tasksLock.Unlock()

	if _, ok := d.tasks[cfg.ID]; ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var taskConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&taskConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	}

	if taskConfig.Image == "" {
		return nil, nil, fmt.Errorf("image is required")
	}

	d.logger.Debug("decoded task config",
		"image", taskConfig.Image,
		"command", taskConfig.Command,
		"args", taskConfig.Args,
		"work_dir", taskConfig.WorkDir,
		"bind_sockets", taskConfig.BindSockets,
		"force_pull", taskConfig.ForcePull,
	)

	image := taskConfig.Image

	d.emitEvent(cfg, "Downloading image: "+image)
	d.logger.Info("starting image pull",
		"image", image,
		"force_pull", taskConfig.ForcePull,
		"backend", d.backend.Name(),
	)
	pullCtx, pullCancel := context.WithTimeout(d.ctx, buildahTimeout)
	defer pullCancel()
	if err := d.backend.Pull(pullCtx, image, taskConfig.ForcePull); err != nil {
		d.emitEvent(cfg, fmt.Sprintf("Failed to download image %s: %v", image, err))
		errMsg := fmt.Errorf("failed to pull image %s: %v", image, err)
		d.logger.Error("image pull failed", "image", image, "error", err)
		return nil, nil, errMsg
	}
	d.emitEvent(cfg, "Image downloaded: "+image)
	d.logger.Info("image pull succeeded", "image", image)

	useDefaultCommand := taskConfig.Command == ""
	useDefaultArgs := len(taskConfig.Args) == 0
	useDefaultWorkDir := taskConfig.WorkDir == ""

	if useDefaultCommand || useDefaultArgs || useDefaultWorkDir {
		d.logger.Debug("inspecting image for Entrypoint/Cmd/WorkDir defaults",
			"image", image,
			"use_default_command", useDefaultCommand,
			"use_default_args", useDefaultArgs,
			"use_default_work_dir", useDefaultWorkDir,
		)
		inspectCtx, inspectCancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer inspectCancel()
		imgConfig, err := d.backend.Inspect(inspectCtx, image)
		if err != nil {
			d.logger.Warn("failed to inspect image metadata — falling back to hardcoded defaults",
				"image", image, "error", err,
			)
		} else {
			d.logger.Debug("image metadata",
				"image", image,
				"entrypoint", imgConfig.Entrypoint,
				"cmd", imgConfig.Cmd,
				"work_dir", imgConfig.WorkDir,
			)

			if useDefaultCommand {
				if len(imgConfig.Entrypoint) > 0 {
					taskConfig.Command = imgConfig.Entrypoint[0]
					d.logger.Debug("using ENTRYPOINT[0] as command",
						"entrypoint0", imgConfig.Entrypoint[0],
					)
					if useDefaultArgs {
						taskConfig.Args = append(imgConfig.Entrypoint[1:], imgConfig.Cmd...)
						d.logger.Debug("using ENTRYPOINT[1:]+CMD as args",
							"args", taskConfig.Args,
						)
					}
				} else if len(imgConfig.Cmd) > 0 {
					taskConfig.Command = imgConfig.Cmd[0]
					d.logger.Debug("using CMD[0] as command (no ENTRYPOINT)",
						"cmd0", imgConfig.Cmd[0],
					)
					if useDefaultArgs {
						taskConfig.Args = imgConfig.Cmd[1:]
						d.logger.Debug("using CMD[1:] as args", "args", taskConfig.Args)
					}
				} else {
					d.logger.Debug("image has no ENTRYPOINT or CMD, falling back to /bin/sh")
				}
			} else if useDefaultArgs && len(imgConfig.Entrypoint) > 0 {
				taskConfig.Args = imgConfig.Cmd
				d.logger.Debug("user provided command but not args — using image CMD as default args",
					"cmd", imgConfig.Cmd,
				)
			}

			if useDefaultWorkDir && imgConfig.WorkDir != "" {
				taskConfig.WorkDir = imgConfig.WorkDir
				d.logger.Debug("using image WORKDIR", "work_dir", taskConfig.WorkDir)
			}
		}

		if taskConfig.Command == "" {
			taskConfig.Command = "/bin/sh"
			d.logger.Debug("command still empty after inspection, defaulting to /bin/sh")
		}
	}

	d.logger.Info("resolved task command",
		"image", image,
		"command", taskConfig.Command,
		"args", taskConfig.Args,
		"work_dir", taskConfig.WorkDir,
		"from_image_defaults", useDefaultCommand || useDefaultArgs || useDefaultWorkDir,
	)

	// Create per-task extraction directory inside the allocation directory
	rootfsDir := filepath.Join(cfg.AllocDir, cfg.Name, "rootfs")
	d.logger.Info("extracting image layers to per-allocation rootfs", "image", image, "rootfs", rootfsDir)

	fromCtx, fromCancel := context.WithTimeout(d.ctx, buildahTimeout)
	defer fromCancel()
	d.emitEvent(cfg, "Extracting image layers: "+image)
	containerName, err := d.backend.From(fromCtx, image, rootfsDir)
	if err != nil {
		d.emitEvent(cfg, fmt.Sprintf("Failed to extract image %s: %v", image, err))
		errMsg := fmt.Errorf("failed to extract image %s: %v", image, err)
		d.logger.Error("image extraction failed", "image", image, "error", err)
		return nil, nil, errMsg
	}
	d.emitEvent(cfg, "Image extracted: "+containerName)
	d.logger.Info("image extracted", "container_id", containerName, "image", image)

	mountCtx, mountCancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer mountCancel()
	d.emitEvent(cfg, "Mounting image rootfs")
	d.logger.Debug("mounting container rootfs", "container_id", containerName)
	mountPoint, err := d.backend.Mount(mountCtx, containerName)
	if err != nil {
		d.emitEvent(cfg, fmt.Sprintf("Failed to mount image rootfs: %v", err))
		d.logger.Error("mount failed, cleaning up container", "container_id", containerName, "error", err)
		rmCtx, rmCancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer rmCancel()
		d.backend.Remove(rmCtx, containerName)
		return nil, nil, fmt.Errorf("failed to mount container %s: %v", containerName, err)
	}
	d.emitEvent(cfg, "Image rootfs mounted")
	d.logger.Info("chroot rootfs ready", "mountpoint", mountPoint, "container_id", containerName)

	selfExe, err := os.Executable()
	if err != nil {
		d.emitEvent(cfg, fmt.Sprintf("Failed to find own executable: %v", err))
		d.logger.Error("cannot determine own executable path", "error", err)
		cleanupCtx, cleanupCancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer cleanupCancel()
		d.backend.Unmount(cleanupCtx, containerName)
		d.backend.Remove(cleanupCtx, containerName)
		return nil, nil, fmt.Errorf("failed to get self path: %v", err)
	}
	d.logger.Debug("self executable path", "path", selfExe)

	argsJSON, _ := json.Marshal(taskConfig.Args)
	argsB64 := base64.StdEncoding.EncodeToString(argsJSON)
	socketsJSON, _ := json.Marshal(taskConfig.BindSockets)
	socketsB64 := base64.StdEncoding.EncodeToString(socketsJSON)

	// Bind-mount task directories from Nomad into the chroot.
	// Nomad's template stanza writes rendered templates to /local and /secrets,
	// so these mounts make template-rendered content available inside the chroot.
	taskDir := filepath.Join(cfg.AllocDir, cfg.Name)
	dirs := map[string]string{
		"/alloc":   filepath.Join(cfg.AllocDir, "alloc"),
		"/local":   filepath.Join(taskDir, "local"),
		"/secrets": filepath.Join(taskDir, "secrets"),
	}
	d.logger.Info("bind-mounting Nomad directories into chroot",
		"alloc_dir", dirs["/alloc"],
		"local_dir", dirs["/local"],
		"secrets_dir", dirs["/secrets"],
		"note", "template stanzas write to /local and /secrets — they are available inside the chroot",
	)
	dirsJSON, _ := json.Marshal(dirs)
	dirsB64 := base64.StdEncoding.EncodeToString(dirsJSON)

	if len(taskConfig.BindSockets) > 0 {
		d.logger.Info("bind-mounting host sockets into chroot",
			"sockets", taskConfig.BindSockets,
		)
	} else {
		d.logger.Debug("no host sockets to bind-mount")
	}

	env := cfg.EnvList()

	// Network isolation: if Nomad provides a network namespace (bridge/group mode),
	// pass the path so the chroot process enters it via setns(CLONE_NEWNET)
	netnsPath := ""
	if cfg.NetworkIsolation != nil && cfg.NetworkIsolation.Path != "" {
		netnsPath = cfg.NetworkIsolation.Path
		d.logger.Info("task uses network namespace", "netns_path", netnsPath, "mode", cfg.NetworkIsolation.Mode)
	} else {
		d.logger.Debug("no network namespace for this task")
	}

	env = append(env,
		"_OCI_CHROOT_EXEC=1",
		"_OCI_CHROOT_MOUNTPOINT="+mountPoint,
		"_OCI_CHROOT_COMMAND="+taskConfig.Command,
		"_OCI_CHROOT_ARGS="+argsB64,
		"_OCI_CHROOT_WORKDIR="+taskConfig.WorkDir,
		"_OCI_CHROOT_BIND_SOCKETS="+socketsB64,
		"_OCI_CHROOT_DIRS="+dirsB64,
		"_OCI_CHROOT_NETNS="+netnsPath,
	)
	d.logger.Debug("task environment prepared",
		"env_count", len(env),
		"alloc_id", cfg.AllocID,
		"task_name", cfg.Name,
		"work_dir", taskConfig.WorkDir,
	)

	cleanup := func() {
		d.logger.Debug("cleaning up container after failed start", "container_id", containerName)
		c, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer cancel()
		d.backend.Unmount(c, containerName)
		d.backend.Remove(c, containerName)
	}

	stdoutW, err := os.OpenFile(cfg.StdoutPath, os.O_WRONLY, 0)
	if err != nil {
		d.emitEvent(cfg, fmt.Sprintf("Failed to open stdout: %v", err))
		d.logger.Error("failed to open stdout FIFO", "path", cfg.StdoutPath, "error", err)
		cleanup()
		return nil, nil, fmt.Errorf("failed to open stdout FIFO: %v", err)
	}
	stderrW, err := os.OpenFile(cfg.StderrPath, os.O_WRONLY, 0)
	if err != nil {
		d.emitEvent(cfg, fmt.Sprintf("Failed to open stderr: %v", err))
		d.logger.Error("failed to open stderr FIFO", "path", cfg.StderrPath, "error", err)
		stdoutW.Close()
		cleanup()
		return nil, nil, fmt.Errorf("failed to open stderr FIFO: %v", err)
	}
	d.logger.Debug("stdout/stderr FIFOs opened",
		"stdout", cfg.StdoutPath,
		"stderr", cfg.StderrPath,
	)

	cmd := exec.Command(selfExe)
	cmd.Env = env
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.Stdin = nil

	d.emitEvent(cfg, "Starting chroot task")
	d.logger.Info("spawning chroot process",
		"pid", "pending",
		"command", taskConfig.Command,
		"args", taskConfig.Args,
		"mountpoint", mountPoint,
		"container_id", containerName,
	)

	if err := cmd.Start(); err != nil {
		d.emitEvent(cfg, fmt.Sprintf("Failed to spawn chroot process: %v", err))
		d.logger.Error("failed to spawn chroot process", "error", err)
		stdoutW.Close()
		stderrW.Close()
		cleanup()
		return nil, nil, fmt.Errorf("failed to start task: %v", err)
	}
	stdoutW.Close()
	stderrW.Close()

	d.logger.Info("chroot process running",
		"pid", cmd.Process.Pid,
		"container_id", containerName,
		"image", image,
		"backend", d.backend.Name(),
		"command", taskConfig.Command,
		"args", taskConfig.Args,
	)

	handle := &taskHandle{
		taskConfig:    cfg,
		proc:          cmd.Process,
		state:         drivers.TaskStateRunning,
		startedAt:     time.Now(),
		containerName: containerName,
		mountPoint:    mountPoint,
		imageRef:      image,
		netnsPath:     netnsPath,
		logger:        d.logger,
		ch:            make(chan *drivers.ExitResult, 1),
		doneCh:        make(chan struct{}),
	}

	go handle.monitor(context.Background())

	d.tasks[cfg.ID] = handle

	d.emitEvent(cfg, "Task started — running "+taskConfig.Command)

	taskHandle := handle.toHandle()
	return taskHandle, nil, nil
}

func (d *OCIDriver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	d.tasksLock.Lock()
	handle, ok := d.tasks[taskID]
	d.tasksLock.Unlock()

	if !ok {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	return handle.waitCh(), nil
}

func (d *OCIDriver) StopTask(taskID string, timeout time.Duration, signal string) error {
	d.logger.Debug("StopTask called", "task_id", taskID, "timeout", timeout, "signal", signal)

	d.tasksLock.Lock()
	handle, ok := d.tasks[taskID]
	d.tasksLock.Unlock()

	if !ok {
		d.logger.Warn("StopTask: task not found", "task_id", taskID)
		return fmt.Errorf("task not found: %s", taskID)
	}

	if handle.done {
		d.logger.Debug("StopTask: task already finished", "task_id", taskID)
		return nil
	}

	sig, err := signalFromString(signal)
	if err != nil {
		d.logger.Warn("invalid signal, falling back to SIGTERM", "signal", signal, "error", err)
		sig = syscall.SIGTERM
	}

	d.logger.Info("sending signal to task", "task_id", taskID, "signal", sig, "pid", handle.proc.Pid)
	handle.proc.Signal(sig)
	if timeout > 0 {
		d.logger.Debug("will force-kill after timeout", "task_id", taskID, "timeout", timeout)
		go func() {
			time.Sleep(timeout)
			if !handle.done {
				d.logger.Warn("timeout reached, force-killing task", "task_id", taskID)
				handle.shutdown()
			}
		}()
	}

	return nil
}

func (d *OCIDriver) DestroyTask(taskID string, force bool) error {
	d.logger.Debug("DestroyTask called", "task_id", taskID, "force", force)

	d.tasksLock.Lock()
	handle, ok := d.tasks[taskID]
	if ok {
		delete(d.tasks, taskID)
	}
	d.tasksLock.Unlock()

	if !ok {
		d.logger.Debug("DestroyTask: no handle found for task, nothing to do", "task_id", taskID)
		return nil
	}

	if !handle.done {
		d.logger.Info("shutting down task process", "task_id", taskID, "pid", handle.proc.Pid)
		handle.shutdown()
	} else {
		d.logger.Debug("task already finished, skipping shutdown", "task_id", taskID)
	}

	if handle.containerName != "" {
		d.logger.Info("cleaning up image container",
			"task_id", taskID,
			"container_id", handle.containerName,
			"mountpoint", handle.mountPoint,
		)
		ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer cancel()
		if err := d.backend.Unmount(ctx, handle.containerName); err != nil {
			d.logger.Warn("unmount failed during cleanup", "container_id", handle.containerName, "error", err)
		}
		if err := d.backend.Remove(ctx, handle.containerName); err != nil {
			d.logger.Warn("remove failed during cleanup", "container_id", handle.containerName, "error", err)
		}
		d.logger.Info("container cleaned up", "container_id", handle.containerName)
	} else {
		d.logger.Debug("no container to clean up", "task_id", taskID)
	}

	return nil
}

func (d *OCIDriver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	d.tasksLock.Lock()
	handle, ok := d.tasks[taskID]
	d.tasksLock.Unlock()

	if !ok {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	status := &drivers.TaskStatus{
		ID:          taskID,
		Name:        handle.taskConfig.Name,
		State:       handle.state,
		StartedAt:   handle.startedAt,
		CompletedAt: handle.completedAt,
		ExitResult:  handle.exitResult,
		DriverAttributes: map[string]string{
			"pid": strconv.Itoa(handle.proc.Pid),
		},
	}

	return status, nil
}

func (d *OCIDriver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *cstructs.TaskResourceUsage, error) {
	handle, ok := d.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	ch := make(chan *cstructs.TaskResourceUsage, 1)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if handle.done {
					return
				}
				usage := d.getResourceUsage(handle.proc.Pid)
				select {
				case ch <- usage:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			case <-d.ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

func (d *OCIDriver) getResourceUsage(pid int) *cstructs.TaskResourceUsage {
	rusage := &cstructs.TaskResourceUsage{
		ResourceUsage: &cstructs.ResourceUsage{
			MemoryStats: &cstructs.MemoryStats{
				RSS:      0,
				Measured: []string{"RSS"},
			},
			CpuStats: &cstructs.CpuStats{
				Measured: []string{"Percent"},
			},
		},
		Timestamp: time.Now().UTC().UnixNano(),
	}

	procDir := fmt.Sprintf("/proc/%d", pid)

	statmBytes, err := os.ReadFile(filepath.Join(procDir, "statm"))
	if err == nil {
		statmFields := strings.Fields(string(statmBytes))
		if len(statmFields) >= 2 {
			rssPages, _ := strconv.ParseUint(statmFields[1], 10, 64)
			rusage.ResourceUsage.MemoryStats.RSS = rssPages * uint64(os.Getpagesize())
			rusage.ResourceUsage.MemoryStats.MaxUsage = rusage.ResourceUsage.MemoryStats.RSS
		}
	}

	statBytes, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err == nil {
		fields := strings.Fields(string(statBytes))
		if len(fields) >= 15 {
			utime, _ := strconv.ParseInt(fields[13], 10, 64)
			stime, _ := strconv.ParseInt(fields[14], 10, 64)
			totalCPU := float64(utime+stime) / 100.0
			rusage.ResourceUsage.CpuStats.SystemMode = totalCPU
			rusage.ResourceUsage.CpuStats.UserMode = totalCPU
			rusage.ResourceUsage.CpuStats.Percent = totalCPU
		}
	}

	return rusage
}

func (d *OCIDriver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.taskEvents, nil
}

func (d *OCIDriver) emitEvent(cfg *drivers.TaskConfig, message string) {
	event := &drivers.TaskEvent{
		TaskID:    cfg.ID,
		TaskName:  cfg.Name,
		AllocID:   cfg.AllocID,
		Timestamp: time.Now(),
		Message:   message,
	}
	select {
	case d.taskEvents <- event:
	default:
		d.logger.Warn("task event channel full, dropping event", "message", message)
	}
}

func (d *OCIDriver) SignalTask(taskID string, signal string) error {
	d.logger.Debug("SignalTask called", "task_id", taskID, "signal", signal)

	d.tasksLock.Lock()
	handle, ok := d.tasks[taskID]
	d.tasksLock.Unlock()

	if !ok {
		d.logger.Warn("SignalTask: task not found", "task_id", taskID)
		return fmt.Errorf("task not found: %s", taskID)
	}

	if handle.done {
		d.logger.Debug("SignalTask: task already finished", "task_id", taskID)
		return nil
	}

	sig, err := signalFromString(signal)
	if err != nil {
		d.logger.Error("SignalTask: invalid signal", "signal", signal, "error", err)
		return fmt.Errorf("invalid signal %q: %v", signal, err)
	}

	d.logger.Info("sending signal to task", "task_id", taskID, "signal", sig, "pid", handle.proc.Pid)
	if err := handle.proc.Signal(sig); err != nil {
		d.logger.Error("SignalTask: failed to send signal", "task_id", taskID, "signal", sig, "error", err)
		return fmt.Errorf("failed to send signal %s to task %s: %v", signal, taskID, err)
	}

	return nil
}

func (d *OCIDriver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	d.logger.Debug("ExecTask called", "task_id", taskID, "cmd", cmd, "timeout", timeout)

	d.tasksLock.Lock()
	handle, ok := d.tasks[taskID]
	d.tasksLock.Unlock()

	if !ok {
		d.logger.Warn("ExecTask: task not found", "task_id", taskID)
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	if handle.done {
		d.logger.Warn("ExecTask: task already finished", "task_id", taskID)
		return nil, fmt.Errorf("task %s is not running", taskID)
	}

	if len(cmd) == 0 || cmd[0] == "" {
		return nil, fmt.Errorf("exec command must not be empty")
	}

	// Resolve mount point: prefer handle, fall back to env
	mountPoint := handle.mountPoint
	if mountPoint == "" {
		d.logger.Error("ExecTask: mount point unknown for task", "task_id", taskID)
		return nil, fmt.Errorf("mount point unknown for task %s", taskID)
	}

	// Serialize args (everything after cmd[0])
	var execArgs []string
	if len(cmd) > 1 {
		execArgs = cmd[1:]
	}
	argsJSON, err := json.Marshal(execArgs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal exec args: %v", err)
	}
	argsB64 := base64.StdEncoding.EncodeToString(argsJSON)

	selfExe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot find own executable: %v", err)
	}

	// Build environment for the exec process
	env := handle.taskConfig.EnvList()
	env = append(env,
		"_OCI_CHROOT_EXEC=1",
		"_OCI_CHROOT_MOUNTPOINT="+mountPoint,
		"_OCI_CHROOT_COMMAND="+cmd[0],
		"_OCI_CHROOT_ARGS="+argsB64,
		"_OCI_CHROOT_WORKDIR=",
	)

	// Enter same network namespace if the task uses one
	if handle.netnsPath != "" {
		env = append(env, "_OCI_CHROOT_NETNS="+handle.netnsPath)
	}

	// Don't bind-mount sockets/dirs for exec — the rootfs is already set up
	env = append(env,
		"_OCI_CHROOT_BIND_SOCKETS=",
		"_OCI_CHROOT_DIRS=",
	)

	d.logger.Debug("ExecTask: spawning chroot executor",
		"command", cmd[0],
		"args", execArgs,
		"mountpoint", mountPoint,
		"netns", handle.netnsPath,
	)

	// Create pipes for stdout and stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %v", err)
	}
	defer stdoutR.Close()

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutW.Close()
		return nil, fmt.Errorf("failed to create stderr pipe: %v", err)
	}
	defer stderrR.Close()

	// Spawn the chroot executor process
	execCmd := exec.Command(selfExe)
	execCmd.Env = env
	execCmd.Stdout = stdoutW
	execCmd.Stderr = stderrW
	execCmd.Stdin = nil

	if err := execCmd.Start(); err != nil {
		stdoutW.Close()
		stderrW.Close()
		return nil, fmt.Errorf("failed to spawn exec process: %v", err)
	}
	stdoutW.Close()
	stderrW.Close()

	// Read output with timeout
	var stdout, stderr []byte
	var waitErr error
	done := make(chan struct{})
	go func() {
		stdout, _ = io.ReadAll(stdoutR)
		stderr, _ = io.ReadAll(stderrR)
		waitErr = execCmd.Wait()
		close(done)
	}()

	if timeout > 0 {
		select {
		case <-done:
		case <-time.After(timeout):
			execCmd.Process.Kill()
			<-done
		}
	} else {
		<-done
	}

	exitCode := 0
	var signal int32
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				signal = int32(ws.Signal())
			}
		} else {
			// Process was killed or other error
			exitCode = -1
		}
	}

	d.logger.Debug("ExecTask: completed",
		"exit_code", exitCode,
		"signal", signal,
		"stdout_len", len(stdout),
		"stderr_len", len(stderr),
	)

	return &drivers.ExecTaskResult{
		Stdout: stdout,
		Stderr: stderr,
		ExitResult: &drivers.ExitResult{
			ExitCode: exitCode,
			Signal:   int(signal),
		},
	}, nil
}

func (d *OCIDriver) ExecTaskStreaming(ctx context.Context, taskID string, execOptions *drivers.ExecOptions) (*drivers.ExitResult, error) {
	d.logger.Debug("ExecTaskStreaming called", "task_id", taskID, "cmd", execOptions.Command)

	d.tasksLock.Lock()
	handle, ok := d.tasks[taskID]
	d.tasksLock.Unlock()

	if !ok {
		d.logger.Warn("ExecTaskStreaming: task not found", "task_id", taskID)
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	if handle.done {
		d.logger.Warn("ExecTaskStreaming: task already finished", "task_id", taskID)
		return nil, fmt.Errorf("task %s is not running", taskID)
	}

	if len(execOptions.Command) == 0 || execOptions.Command[0] == "" {
		return nil, fmt.Errorf("exec command must not be empty")
	}

	mountPoint := handle.mountPoint
	if mountPoint == "" {
		d.logger.Error("ExecTaskStreaming: mount point unknown for task", "task_id", taskID)
		return nil, fmt.Errorf("mount point unknown for task %s", taskID)
	}

	cmd := execOptions.Command
	var execArgs []string
	if len(cmd) > 1 {
		execArgs = cmd[1:]
	}
	argsJSON, err := json.Marshal(execArgs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal exec args: %v", err)
	}
	argsB64 := base64.StdEncoding.EncodeToString(argsJSON)

	selfExe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot find own executable: %v", err)
	}

	env := handle.taskConfig.EnvList()
	env = append(env,
		"_OCI_CHROOT_EXEC=1",
		"_OCI_CHROOT_MOUNTPOINT="+mountPoint,
		"_OCI_CHROOT_COMMAND="+cmd[0],
		"_OCI_CHROOT_ARGS="+argsB64,
		"_OCI_CHROOT_WORKDIR=",
	)

	if handle.netnsPath != "" {
		env = append(env, "_OCI_CHROOT_NETNS="+handle.netnsPath)
	}

	env = append(env,
		"_OCI_CHROOT_BIND_SOCKETS=",
		"_OCI_CHROOT_DIRS=",
	)

	d.logger.Debug("ExecTaskStreaming: spawning chroot executor",
		"command", cmd[0],
		"args", execArgs,
		"mountpoint", mountPoint,
		"netns", handle.netnsPath,
	)

	if execOptions.Tty {
		env = append(env, "_OCI_CHROOT_QUIET=1")
		return d.execTTY(ctx, selfExe, env, execOptions)
	}

	return d.execPipe(ctx, selfExe, env, execOptions)
}

func (d *OCIDriver) execTTY(ctx context.Context, selfExe string, env []string, execOptions *drivers.ExecOptions) (*drivers.ExitResult, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open PTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	select {
	case resize := <-execOptions.ResizeCh:
		pty.Setsize(master, &pty.Winsize{
			Rows: uint16(resize.Height),
			Cols: uint16(resize.Width),
		})
	default:
	}

	execCmd := exec.CommandContext(ctx, selfExe)
	execCmd.Env = env
	execCmd.Stdin = slave
	execCmd.Stdout = slave
	execCmd.Stderr = slave
	execCmd.SysProcAttr = &syscall.SysProcAttr{
		Setctty: true,
		Ctty:    0,
		Setsid:  true,
	}

	if err := execCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start exec process: %v", err)
	}

	slave.Close()

	go func() {
		for resize := range execOptions.ResizeCh {
			pty.Setsize(master, &pty.Winsize{
				Rows: uint16(resize.Height),
				Cols: uint16(resize.Width),
			})
		}
	}()

	if execOptions.Stdin != nil {
		go func() {
			io.Copy(master, execOptions.Stdin)
		}()
	}

	go func() {
		io.Copy(execOptions.Stdout, master)
	}()

	err = execCmd.Wait()

	exitCode := 0
	var sig int32
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				sig = int32(ws.Signal())
			}
		} else {
			exitCode = -1
		}
	}

	d.logger.Debug("ExecTaskStreaming: completed",
		"exit_code", exitCode,
		"signal", sig,
	)

	return &drivers.ExitResult{
		ExitCode: exitCode,
		Signal:   int(sig),
	}, nil
}

func (d *OCIDriver) execPipe(ctx context.Context, selfExe string, env []string, execOptions *drivers.ExecOptions) (*drivers.ExitResult, error) {
	execCmd := exec.CommandContext(ctx, selfExe)
	execCmd.Env = env
	execCmd.Stdout = execOptions.Stdout
	execCmd.Stderr = execOptions.Stderr
	execCmd.Stdin = execOptions.Stdin

	if err := execCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn exec process: %v", err)
	}

	err := execCmd.Wait()

	exitCode := 0
	var sig int32
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				sig = int32(ws.Signal())
			}
		} else {
			exitCode = -1
		}
	}

	return &drivers.ExitResult{
		ExitCode: exitCode,
		Signal:   int(sig),
	}, nil
}

func (d *OCIDriver) Shutdown() error {
	d.logger.Info("Shutdown called — stopping all tasks and cleaning up")
	d.cancel()

	d.tasksLock.Lock()
	defer d.tasksLock.Unlock()

	taskCount := len(d.tasks)
	d.logger.Info("shutting down running tasks", "count", taskCount)

	for id, handle := range d.tasks {
		d.logger.Debug("shutting down task", "task_id", id, "pid", handle.proc.Pid)
		if !handle.done {
			handle.shutdown()
		}
		if handle.containerName != "" {
			d.logger.Debug("cleaning up container", "task_id", id, "container_id", handle.containerName)
			ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
			defer cancel()
			if err := d.backend.Unmount(ctx, handle.containerName); err != nil {
				d.logger.Warn("unmount failed during shutdown", "container_id", handle.containerName, "error", err)
			}
			if err := d.backend.Remove(ctx, handle.containerName); err != nil {
				d.logger.Warn("remove failed during shutdown", "container_id", handle.containerName, "error", err)
			}
		}
	}

	d.logger.Info("shutdown complete")
	return nil
}
