package driver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
)

const pluginVersion = "0.1.0"

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
	Enabled    bool `codec:"enabled"`
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

	var config Config
	if len(c.PluginConfig) > 0 {
		if err := base.MsgPackDecode(c.PluginConfig, &config); err != nil {
			return err
		}
	}
	d.config = &config

	// Initialize buildah backend based on config
	if d.config.HostBuildah {
		d.logger.Info("using host buildah binary")
		d.backend = &cliBuildahBackend{}
	} else {
		d.logger.Info("using embedded buildah (go-containerregistry)")
		backend, err := newEmbeddedBuildahBackend()
		if err != nil {
			return fmt.Errorf("failed to create embedded buildah backend: %v", err)
		}
		d.backend = backend
	}

	if d.config.Enabled {
		d.logger.Info(d.pluginName + " driver enabled")
	} else {
		d.logger.Info(d.pluginName + " driver disabled")
	}

	return nil
}

func (d *OCIDriver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (d *OCIDriver) Capabilities() (*drivers.Capabilities, error) {
	return &drivers.Capabilities{
		SendSignals: false,
		Exec:        false,
		FSIsolation: fsisolation.Chroot,
		NetIsolationModes: []drivers.NetIsolationMode{
			drivers.NetIsolationModeHost,
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

		for {
			health := drivers.HealthStateHealthy
			desc := drivers.DriverHealthy
			attrs := map[string]*pstructs.Attribute{}

			if d.backend != nil {
				buildahVer, _ := d.backend.Version()
				attrKey := "driver." + d.pluginName
				attrs = map[string]*pstructs.Attribute{
					attrKey:                        pstructs.NewStringAttribute("1"),
					attrKey + ".version":           pstructs.NewStringAttribute(pluginVersion),
					attrKey + ".buildah":           pstructs.NewStringAttribute(buildahVer),
					attrKey + ".buildah_backend":   pstructs.NewStringAttribute(d.backend.Name()),
				}

				if ok, failDesc := d.backend.Available(); !ok {
					health = drivers.HealthStateUndetected
					desc = failDesc
				}
			} else {
				health = drivers.HealthStateUndetected
				desc = "backend not initialized (SetConfig not called)"
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

	image := taskConfig.Image

	pullCtx, pullCancel := context.WithTimeout(d.ctx, buildahTimeout)
	defer pullCancel()
	d.logger.Info("pulling image", "image", image)
	if err := d.backend.Pull(pullCtx, image, taskConfig.ForcePull); err != nil {
		return nil, nil, fmt.Errorf("failed to pull image %s: %v", image, err)
	}

	useDefaultCommand := taskConfig.Command == ""
	useDefaultArgs := len(taskConfig.Args) == 0

	if useDefaultCommand || useDefaultArgs {
		inspectCtx, inspectCancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer inspectCancel()
		imgConfig, err := d.backend.Inspect(inspectCtx, image)
		if err != nil {
			d.logger.Warn("failed to inspect image, using hardcoded defaults", "image", image, "err", err)
		} else {
			if useDefaultCommand {
				if len(imgConfig.Entrypoint) > 0 {
					taskConfig.Command = imgConfig.Entrypoint[0]
					if useDefaultArgs {
						taskConfig.Args = append(imgConfig.Entrypoint[1:], imgConfig.Cmd...)
					}
				} else if len(imgConfig.Cmd) > 0 {
					taskConfig.Command = imgConfig.Cmd[0]
					if useDefaultArgs {
						taskConfig.Args = imgConfig.Cmd[1:]
					}
				}
			} else if useDefaultArgs && len(imgConfig.Entrypoint) > 0 {
				taskConfig.Args = imgConfig.Cmd
			}
		}

		if taskConfig.Command == "" {
			taskConfig.Command = "/bin/sh"
		}
	}

	d.logger.Info("resolved command",
		"image", image,
		"command", taskConfig.Command,
		"args", taskConfig.Args,
		"default_command", useDefaultCommand,
		"default_args", useDefaultArgs,
	)

	fromCtx, fromCancel := context.WithTimeout(d.ctx, buildahTimeout)
	defer fromCancel()
	d.logger.Info("creating container from image", "image", image)
	containerName, err := d.backend.From(fromCtx, image)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create container from %s: %v", image, err)
	}
	d.logger.Info("container created", "name", containerName)

	mountCtx, mountCancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer mountCancel()
	mountPoint, err := d.backend.Mount(mountCtx, containerName)
	if err != nil {
		rmCtx, rmCancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer rmCancel()
		d.backend.Remove(rmCtx, containerName)
		return nil, nil, fmt.Errorf("failed to mount container %s: %v", containerName, err)
	}
	d.logger.Info("container mounted", "mountpoint", mountPoint)

	selfExe, err := os.Executable()
	if err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer cleanupCancel()
		d.backend.Unmount(cleanupCtx, containerName)
		d.backend.Remove(cleanupCtx, containerName)
		return nil, nil, fmt.Errorf("failed to get self path: %v", err)
	}

	argsJSON, _ := json.Marshal(taskConfig.Args)
	argsB64 := base64.StdEncoding.EncodeToString(argsJSON)
	socketsJSON, _ := json.Marshal(taskConfig.BindSockets)
	socketsB64 := base64.StdEncoding.EncodeToString(socketsJSON)

	taskDir := filepath.Join(cfg.AllocDir, cfg.Name)
	dirs := map[string]string{
		"/alloc":   filepath.Join(cfg.AllocDir, "alloc"),
		"/local":   filepath.Join(taskDir, "local"),
		"/secrets": filepath.Join(taskDir, "secrets"),
	}
	dirsJSON, _ := json.Marshal(dirs)
	dirsB64 := base64.StdEncoding.EncodeToString(dirsJSON)

	env := cfg.EnvList()
	env = append(env,
		"_OCI_CHROOT_EXEC=1",
		"_OCI_CHROOT_MOUNTPOINT="+mountPoint,
		"_OCI_CHROOT_COMMAND="+taskConfig.Command,
		"_OCI_CHROOT_ARGS="+argsB64,
		"_OCI_CHROOT_BIND_SOCKETS="+socketsB64,
		"_OCI_CHROOT_DIRS="+dirsB64,
	)

	cleanup := func() {
		c, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer cancel()
		d.backend.Unmount(c, containerName)
		d.backend.Remove(c, containerName)
	}

	stdoutW, err := os.OpenFile(cfg.StdoutPath, os.O_WRONLY, 0)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to open stdout FIFO: %v", err)
	}
	stderrW, err := os.OpenFile(cfg.StderrPath, os.O_WRONLY, 0)
	if err != nil {
		stdoutW.Close()
		cleanup()
		return nil, nil, fmt.Errorf("failed to open stderr FIFO: %v", err)
	}

	cmd := exec.Command(selfExe)
	cmd.Env = env
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		stdoutW.Close()
		stderrW.Close()
		cleanup()
		return nil, nil, fmt.Errorf("failed to start task: %v", err)
	}
	stdoutW.Close()
	stderrW.Close()

	d.logger.Info("task started", "pid", cmd.Process.Pid, "container", containerName, "backend", d.backend.Name())

	handle := &taskHandle{
		taskConfig:    cfg,
		proc:          cmd.Process,
		state:         drivers.TaskStateRunning,
		startedAt:     time.Now(),
		containerName: containerName,
		mountPoint:    mountPoint,
		imageRef:      image,
		logger:        d.logger,
		ch:            make(chan *drivers.ExitResult, 1),
		doneCh:        make(chan struct{}),
	}

	go handle.monitor(context.Background())

	d.tasks[cfg.ID] = handle

	d.taskEvents <- &drivers.TaskEvent{
		TaskID:    cfg.ID,
		TaskName:  cfg.Name,
		AllocID:   cfg.AllocID,
		Timestamp: time.Now(),
		Message:   "task started",
	}

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
	d.tasksLock.Lock()
	handle, ok := d.tasks[taskID]
	d.tasksLock.Unlock()

	if !ok {
		return fmt.Errorf("task not found: %s", taskID)
	}

	if handle.done {
		return nil
	}

	sig := os.Signal(syscall.SIGTERM)
	if signal != "" {
		switch signal {
		case "SIGKILL":
			sig = syscall.SIGKILL
		case "SIGINT":
			sig = syscall.SIGINT
		}
	}

	handle.proc.Signal(sig)
	if timeout > 0 {
		go func() {
			time.Sleep(timeout)
			if !handle.done {
				handle.shutdown()
			}
		}()
	}

	return nil
}

func (d *OCIDriver) DestroyTask(taskID string, force bool) error {
	d.tasksLock.Lock()
	handle, ok := d.tasks[taskID]
	if ok {
		delete(d.tasks, taskID)
	}
	d.tasksLock.Unlock()

	if !ok {
		return nil
	}

	if !handle.done {
		handle.shutdown()
	}

	if handle.containerName != "" {
		d.logger.Info("cleaning up buildah container", "container", handle.containerName)
		ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer cancel()
		d.backend.Unmount(ctx, handle.containerName)
		d.backend.Remove(ctx, handle.containerName)
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

func (d *OCIDriver) SignalTask(taskID string, signal string) error {
	return fmt.Errorf("SignalTask is not supported by this driver")
}

func (d *OCIDriver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	return nil, fmt.Errorf("ExecTask is not supported by this driver")
}

func (d *OCIDriver) Shutdown() error {
	d.cancel()

	d.tasksLock.Lock()
	defer d.tasksLock.Unlock()

	for _, handle := range d.tasks {
		if !handle.done {
			handle.shutdown()
		}
		if handle.containerName != "" {
			ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
			defer cancel()
			d.backend.Unmount(ctx, handle.containerName)
			d.backend.Remove(ctx, handle.containerName)
		}
	}

	return nil
}
