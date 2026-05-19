package driver

import (
	"context"
	"os"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
)

type taskHandle struct {
	taskConfig  *drivers.TaskConfig
	proc        *os.Process
	state       drivers.TaskState
	startedAt   time.Time
	completedAt time.Time
	exitResult  *drivers.ExitResult

	containerName string
	mountPoint    string
	imageRef      string

	logger hclog.Logger
	ch     chan *drivers.ExitResult
	done   bool
	doneCh chan struct{}
}

func (h *taskHandle) waitCh() <-chan *drivers.ExitResult {
	return h.ch
}

func (h *taskHandle) shutdown() {
	close(h.doneCh)
}

func (h *taskHandle) monitor(ctx context.Context) {
	defer close(h.ch)

	ps := h.proc
	waitCh := make(chan *drivers.ExitResult, 1)

	go func() {
		state, err := ps.Wait()
		if err != nil {
			waitCh <- &drivers.ExitResult{
				ExitCode: 1,
				Err:      err,
			}
			return
		}
		ws := state.Sys().(syscall.WaitStatus)
		exitRes := &drivers.ExitResult{
			ExitCode: ws.ExitStatus(),
		}
		if ws.Signaled() {
			exitRes.Signal = int(ws.Signal())
		}
		waitCh <- exitRes
	}()

	select {
	case res := <-waitCh:
		h.done = true
		h.exitResult = res
		h.completedAt = time.Now()
		h.state = drivers.TaskStateExited
		h.ch <- res
	case <-h.doneCh:
		ps.Signal(syscall.SIGKILL)
		ps.Wait()
		h.done = true
		h.completedAt = time.Now()
		h.state = drivers.TaskStateExited
		h.ch <- &drivers.ExitResult{
			ExitCode: -1,
			Signal:   int(syscall.SIGKILL),
		}
	case <-ctx.Done():
		ps.Signal(syscall.SIGKILL)
		ps.Wait()
		h.done = true
		h.completedAt = time.Now()
		h.state = drivers.TaskStateExited
		h.ch <- &drivers.ExitResult{
			ExitCode: -1,
			Signal:   int(syscall.SIGKILL),
		}
	}
}

func (h *taskHandle) toHandle() *drivers.TaskHandle {
	state := OciChrootTaskState{
		ContainerName: h.containerName,
		MountPoint:    h.mountPoint,
		PID:           h.proc.Pid,
		ImageRef:      h.imageRef,
	}
	handle := drivers.NewTaskHandle(1)
	handle.Config = h.taskConfig
	handle.State = h.state
	handle.SetDriverState(&state)
	return handle
}

func buildHandleFromHandle(th *drivers.TaskHandle, logger hclog.Logger) (*taskHandle, error) {
	var state OciChrootTaskState
	if err := th.GetDriverState(&state); err != nil {
		return nil, err
	}
	logger.Info("recovering task", "pid", state.PID, "container", state.ContainerName)

	proc, err := os.FindProcess(state.PID)
	if err != nil {
		logger.Warn("cannot find process", "pid", state.PID, "err", err)
	}

	handle := &taskHandle{
		taskConfig:    th.Config,
		proc:          proc,
		state:         th.State,
		containerName: state.ContainerName,
		mountPoint:    state.MountPoint,
		imageRef:      state.ImageRef,
		startedAt:     time.Now(),
		logger:        logger,
		ch:            make(chan *drivers.ExitResult, 1),
		doneCh:        make(chan struct{}),
	}

	if proc != nil {
		go handle.monitor(context.Background())
	}

	return handle, nil
}

func (d *OCIDriver) recoverTask(handle *drivers.TaskHandle) error {
	h, err := buildHandleFromHandle(handle, d.logger)
	if err != nil {
		return err
	}

	d.tasksLock.Lock()
	defer d.tasksLock.Unlock()
	d.tasks[h.taskConfig.ID] = h
	return nil
}
