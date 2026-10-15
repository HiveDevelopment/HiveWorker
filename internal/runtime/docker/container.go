package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"hivepanel-worker/internal/identity"

	hiveruntime "hivepanel-worker/internal/runtime"

	dockernetwork "github.com/docker/docker/api/types/network"
)

func (r *DockerRuntime) Start(cell hiveruntime.RuntimeCell, onOutput func(line string), onExit func()) error {
	r.mutex.Lock()
	if _, exists := r.containers[cell.ID]; exists {
		r.mutex.Unlock()
		return errors.New("cell already running")
	}
	r.mutex.Unlock()

	if cell.Image == "" {
		return errors.New("docker image is required")
	}

	if cell.WorkingDir == "" {
		cell.WorkingDir = "/home/container"
	}

	ctx := context.Background()

	if err := r.images.Ensure(ctx, cell.Image, onOutput); err != nil {
		return err
	}

	managedUser, _, _, err := identity.Ensure()
	if err != nil {
		return fmt.Errorf("resolve container identity: %w", err)
	}

	instanceAbs, err := filepath.Abs(cell.InstanceDir)
	if err != nil {
		return err
	}

	exposedPorts, portBindings, err := allocationPorts(cell)
	if err != nil {
		return err
	}

	env := []string{}
	for key, value := range cell.Environment {
		env = append(env, key+"="+value)
	}

	containerName := "hivepanel-cell-" + cell.ID

	if err := r.removeExistingContainer(ctx, containerName); err != nil {
		return fmt.Errorf("remove previous Cell container: %w", err)
	}

	resp, err := r.client.ContainerCreate(
		ctx,
		&container.Config{
			Image:        cell.Image,
			User:         managedUser,
			WorkingDir:   cell.WorkingDir,
			Cmd:          shellCommand(cell.Command, cell.Shell),
			Env:          env,
			Tty:          false,
			OpenStdin:    true,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			ExposedPorts: exposedPorts,
			Labels: map[string]string{
				labelManagedBy:      "true",
				"hivepanel.cell_id": cell.ID,
			},
		},
		&container.HostConfig{
			Binds:        []string{instanceAbs + ":" + cell.WorkingDir + ":Z"},
			PortBindings: portBindings,
			Resources:    dockerResources(cell.Limits),
		},
		&dockernetwork.NetworkingConfig{
			EndpointsConfig: map[string]*dockernetwork.EndpointSettings{
				r.networks.Name(): {},
			},
		},
		nil,
		containerName,
	)
	if err != nil {
		return err
	}

	if err := r.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = r.client.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return err
	}
	attach, err := r.client.ContainerAttach(ctx, resp.ID, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: false,
		Stderr: false,
	})
	if err != nil {
		_ = r.client.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return err
	}

	r.mutex.Lock()
	r.containers[cell.ID] = resp.ID
	r.startedAt[cell.ID] = time.Now()
	r.stdin[cell.ID] = attach.Conn
	r.mutex.Unlock()

	logCtx, cancel := context.WithCancel(context.Background())

	r.mutex.Lock()
	r.cancelLogs[cell.ID] = cancel
	r.mutex.Unlock()

	go r.streamLogs(logCtx, resp.ID, onOutput)

	go func() {
		statusCh, errCh := r.client.ContainerWait(context.Background(), resp.ID, container.WaitConditionNotRunning)

		select {
		case <-statusCh:
		case <-errCh:
		}

		r.mutex.Lock()
		delete(r.containers, cell.ID)
		delete(r.startedAt, cell.ID)

		if stdin, exists := r.stdin[cell.ID]; exists {
			_ = stdin.Close()
			delete(r.stdin, cell.ID)
		}

		if cancel, exists := r.cancelLogs[cell.ID]; exists {
			cancel()
			delete(r.cancelLogs, cell.ID)
		}
		r.mutex.Unlock()

		onExit()
	}()

	return nil
}

func (r *DockerRuntime) Stop(id string) error {
	return r.StopConfigured(id, hiveruntime.StopOptions{Command: "stop", Timeout: 30, Signal: "SIGTERM"})
}

func (r *DockerRuntime) StopConfigured(id string, options hiveruntime.StopOptions) error {
	r.mutex.Lock()
	containerID, exists := r.containers[id]
	r.mutex.Unlock()
	if !exists {
		return errors.New("cell is not running")
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > 600 {
		timeout = 600
	}
	signal := options.Signal
	if signal == "" {
		signal = "SIGTERM"
	}
	// Give the console command time to save and exit before sending a signal.
	if options.Command != "" && r.SendCommand(id, options.Command) == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		statusCh, errCh := r.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
		select {
		case <-statusCh:
			cancel()
			return nil
		case err := <-errCh:
			cancel()
			if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				return err
			}
		case <-ctx.Done():
			cancel()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout+5)*time.Second)
	defer cancel()
	// Signal-only servers receive the configured signal immediately, followed
	// by Docker's forced termination only when the grace period expires.
	return r.client.ContainerStop(ctx, containerID, container.StopOptions{Signal: signal, Timeout: &timeout})
}

func (r *DockerRuntime) SendCommand(id string, command string) error {
	r.mutex.Lock()
	stdin, exists := r.stdin[id]
	r.mutex.Unlock()

	if !exists || stdin == nil {
		return errors.New("cell is not running")
	}

	_, err := io.WriteString(stdin, command+"\n")
	return err
}

func (r *DockerRuntime) IsRunning(id string) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	_, exists := r.containers[id]
	return exists
}

func (r *DockerRuntime) StartedAt(id string) *time.Time {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	started, exists := r.startedAt[id]
	if !exists {
		return nil
	}

	return &started
}

func (r *DockerRuntime) removeExistingContainer(ctx context.Context, name string) error {
	inspect, err := r.client.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}

	if inspect.State != nil && inspect.State.Running {
		timeout := 10
		_ = r.client.ContainerStop(ctx, inspect.ID, container.StopOptions{
			Timeout: &timeout,
		})
	}

	return r.client.ContainerRemove(ctx, inspect.ID, container.RemoveOptions{
		Force: true,
	})
}

func (r *DockerRuntime) Recover(cellIDs []string, onOutput func(cellID string, line string), onExit func(cellID string)) error {
	ctx := context.Background()

	for _, cellID := range cellIDs {
		name := "hivepanel-cell-" + cellID

		inspect, err := r.client.ContainerInspect(ctx, name)
		if err != nil {
			continue
		}

		if inspect.State == nil || !inspect.State.Running {
			continue
		}

		logCtx, cancel := context.WithCancel(context.Background())

		attach, err := r.client.ContainerAttach(ctx, inspect.ID, container.AttachOptions{
			Stream: true,
			Stdin:  true,
			Stdout: false,
			Stderr: false,
		})

		r.mutex.Lock()
		r.containers[cellID] = inspect.ID
		r.startedAt[cellID] = time.Now()
		r.cancelLogs[cellID] = cancel

		if err == nil {
			r.stdin[cellID] = attach.Conn
		}

		r.mutex.Unlock()

		go r.streamLogs(logCtx, inspect.ID, func(line string) {
			onOutput(cellID, line)
		})

		go func(id string, containerID string) {
			statusCh, errCh := r.client.ContainerWait(context.Background(), containerID, container.WaitConditionNotRunning)

			select {
			case <-statusCh:
			case <-errCh:
			}

			r.mutex.Lock()
			delete(r.containers, id)
			delete(r.startedAt, id)

			if stdin, exists := r.stdin[id]; exists {
				_ = stdin.Close()
				delete(r.stdin, id)
			}

			if cancel, exists := r.cancelLogs[id]; exists {
				cancel()
				delete(r.cancelLogs, id)
			}

			r.mutex.Unlock()

			onExit(id)
		}(cellID, inspect.ID)
	}

	return nil
}

func dockerResources(limits hiveruntime.Limits) container.Resources {
	resources := container.Resources{
		OomKillDisable: boolPointer(!limits.OOMKiller),
	}

	if limits.MemoryMB > 0 {
		resources.Memory = int64(limits.MemoryMB) * 1024 * 1024
	}

	if limits.CPUPercent > 0 {
		resources.NanoCPUs = int64(limits.CPUPercent) * 10_000_000
	}

	if limits.CPUPinning != "" {
		resources.CpusetCpus = limits.CPUPinning
	}

	if limits.IOWeight > 0 {
		ioWeight := limits.IOWeight

		if ioWeight < 10 {
			ioWeight = 10
		}

		if ioWeight > 1000 {
			ioWeight = 1000
		}

		resources.BlkioWeight = uint16(ioWeight)
	}

	if limits.MemoryMB > 0 {
		memoryBytes := int64(limits.MemoryMB) * 1024 * 1024

		if limits.SwapMB > 0 {
			resources.MemorySwap = memoryBytes + int64(limits.SwapMB)*1024*1024
		} else {
			resources.MemorySwap = memoryBytes
		}
	}

	return resources
}

func boolPointer(value bool) *bool {
	return &value
}

func shellCommand(command string, preferred ...string) []string {
	if len(preferred) > 0 {
		if preferred[0] == "sh" {
			return []string{"sh", "-c", command}
		}
		if preferred[0] == "bash" {
			return []string{"bash", "-lc", command}
		}
	}
	// Preserve Bash login behavior when available; legacy Alpine Combs can use POSIX sh.
	// Pass the original command as an argument, never interpolate it into this launcher.
	return []string{"sh", "-c", `if command -v bash >/dev/null 2>&1; then exec bash -lc "$1"; else exec sh -c "$1"; fi`, "hivepanel-start", command}
}
