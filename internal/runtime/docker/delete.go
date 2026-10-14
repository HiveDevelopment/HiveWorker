package docker

import (
	"context"
	"errors"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"time"
)

func (r *DockerRuntime) Delete(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inspect, err := r.client.ContainerInspect(ctx, "hivepanel-cell-"+id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	if inspect.Config == nil || inspect.Config.Labels[labelManagedBy] != "true" || inspect.Config.Labels["hivepanel.cell_id"] != id {
		return errors.New("refusing to delete a container not owned by this Cell")
	}
	if inspect.State != nil && inspect.State.Running {
		return errors.New("Cell container must be stopped before deletion")
	}
	if err := r.client.ContainerRemove(ctx, inspect.ID, container.RemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if cancel := r.cancelLogs[id]; cancel != nil {
		cancel()
	}
	if input := r.stdin[id]; input != nil {
		_ = input.Close()
	}
	delete(r.containers, id)
	delete(r.cancelLogs, id)
	delete(r.startedAt, id)
	delete(r.stdin, id)
	return nil
}
