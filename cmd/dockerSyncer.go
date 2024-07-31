package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/axtgr/docker-sync/filewatcher"
	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
)

const (
	AppIdentifier               = "docker-sync"
	TemporaryContainerImage     = "hello-world"
	TemporaryContainerMountPath = "/docker-sync-data"
	stopTimeoutInSeconds        = 10
)

func makeTemporaryName() string {
	return AppIdentifier + "-" + uuid.New().String()
}

type TargetType int

const (
	Container = iota
	Service
)

type DockerSyncer struct {
	client             *client.Client
	host               string
	target             string
	targetType         TargetType
	targetPath         string
	restartTarget      bool
	temporaryContainer string
	temporaryVolume    string
}

func NewDockerSyncer(target string, targetPath string, restartTarget bool, host string) (*DockerSyncer, error) {
	return &DockerSyncer{
		host:          host,
		target:        target,
		targetPath:    targetPath,
		restartTarget: restartTarget,
	}, nil
}

func (ds *DockerSyncer) Connect() error {
	var clientOpts []client.Opt

	helper, err := connhelper.GetConnectionHelper(ds.host)
	if err != nil {
		// Not an SSH URL, use default connection
		clientOpts = append(clientOpts, client.WithHost(ds.host))
	} else {
		// SSH URL
		httpClient := &http.Client{
			Transport: &http.Transport{
				DialContext: helper.Dialer,
			},
		}

		clientOpts = append(clientOpts,
			client.WithHTTPClient(httpClient),
			client.WithHost(helper.Host),
			client.WithDialContext(helper.Dialer),
			client.WithAPIVersionNegotiation(),
		)
	}

	client, err := client.NewClientWithOpts(clientOpts...)
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}

	ds.client = client
	return nil
}

func (ds *DockerSyncer) Init() error {
	err := ds.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to docker: %w", err)
	}

	service, err := ds.findTargetService()
	if err != nil {
		return fmt.Errorf("failed to find service %s: %w", ds.target, err)
	}

	if service == "" {
		container, err := ds.findTargetContainer()
		if err != nil {
			return fmt.Errorf("failed to find container %s: %w", ds.target, err)
		}
		if container == "" {
			return fmt.Errorf("failed to find container or service %s", ds.target)
		}

		ds.targetType = Container
		ds.target = container
	} else {
		ds.targetType = Service
		ds.target = service
	}

	if ds.restartTarget && ds.targetType == Service {
		err := ds.createTemporaryContainerWithVolume()
		if err != nil {
			return fmt.Errorf("failed to create a temporary container with a volume: %w", err)
		}
	}

	return nil
}

func (ds *DockerSyncer) Sync(localPath string, op filewatcher.Op) error {
	if ds.targetType == Container && !ds.restartTarget {
		container, err := ds.findTargetContainer()
		if err != nil {
			return fmt.Errorf("failed to find container %s: %w", ds.target, err)
		}

		err = ds.copyToContainer(localPath, container, ds.targetPath)
		if err != nil {
			return fmt.Errorf("failed to copy to container %s: %w", container, err)
		}

		return nil
	}

	if ds.targetType == Container && ds.restartTarget {
		container, err := ds.findTargetContainer()
		if err != nil {
			return fmt.Errorf("failed to find container %s: %w", ds.target, err)
		}

		err = ds.copyToContainer(localPath, container, ds.targetPath)
		if err != nil {
			return fmt.Errorf("failed to copy to container %s: %w", container, err)
		}

		err = ds.restartTargetContainer(true)
		if err != nil {
			return fmt.Errorf("failed to restart container %s: %w", container, err)
		}

		return nil
	}

	if ds.targetType == Service && !ds.restartTarget {
		container, err := ds.getContainerIdForTargetService()
		if err != nil {
			return fmt.Errorf("failed to container ID for service %s: %w", ds.target, err)
		}

		err = ds.copyToContainer(localPath, container, ds.targetPath)
		if err != nil {
			return fmt.Errorf("failed to copy to container %s: %w", container, err)
		}

		return nil
	}

	if ds.targetType == Service && ds.restartTarget {
		err := ds.copyToContainer(localPath, ds.temporaryContainer, TemporaryContainerMountPath)
		if err != nil {
			return fmt.Errorf("failed to copy to temporary container %s: %w", ds.temporaryContainer, err)
		}

		err = ds.restartTargetService(true)
		if err != nil {
			return fmt.Errorf("failed to restart service %s: %w", ds.target, err)
		}
	}

	return nil
}

func (ds *DockerSyncer) Cleanup() error {
	ctx := context.Background()

	if ds.targetType == Container {
		err := ds.restartTargetContainer(false)
		if err != nil {
			return fmt.Errorf("failed to restart target container %s: %w", ds.target, err)
		}
	} else {
		err := ds.restartTargetService(false)
		if err != nil {
			return fmt.Errorf("failed to restart target service: %w", err)
		}
	}

	err := ds.client.ContainerRemove(ctx, ds.temporaryContainer, container.RemoveOptions{
		Force: true,
	})
	if err != nil {
		return fmt.Errorf("failed to remove temporary container %s: %w", ds.temporaryContainer, err)
	}

	err = ds.client.VolumeRemove(ctx, ds.temporaryVolume, true)
	if err != nil {
		return fmt.Errorf("failed to remove temporary volume %s: %w", ds.temporaryVolume, err)
	}

	ds.temporaryContainer = ""
	ds.temporaryVolume = ""

	return nil
}

func (ds *DockerSyncer) findContainerById(needle string) (string, error) {
	containers, err := ds.client.ContainerList(context.Background(), container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("id", needle)),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list containers: %w", err)
	}
	if len(containers) == 0 {
		return "", nil
	}
	return containers[0].ID, nil
}

func (ds *DockerSyncer) findContainerByName(needle string) (string, error) {
	containers, err := ds.client.ContainerList(context.Background(), container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", needle)),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list containers: %w", err)
	}
	if len(containers) == 0 {
		return "", nil
	}
	return containers[0].ID, nil
}

func (ds *DockerSyncer) findTargetContainer() (string, error) {
	id, err := ds.findContainerById(ds.target)
	if err != nil {
		return "", fmt.Errorf("failed to find container by ID or name %s: %w", ds.target, err)
	}
	if id != "" {
		return id, nil
	}
	containerId, err := ds.findContainerByName(ds.target)
	if err != nil {
		return "", fmt.Errorf("failed to find container by ID or name %s: %w", ds.target, err)
	}
	return containerId, nil
}

func (ds *DockerSyncer) findServiceById(needle string) (string, error) {
	services, err := ds.client.ServiceList(context.Background(), types.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("id", needle)),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list services: %w", err)
	}
	if len(services) == 0 {
		return "", nil
	}
	return services[0].ID, nil
}

func (ds *DockerSyncer) findServiceByName(needle string) (string, error) {
	services, err := ds.client.ServiceList(context.Background(), types.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("name", needle)),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list services: %w", err)
	}
	if len(services) == 0 {
		return "", nil
	}
	return services[0].ID, nil
}

func (ds *DockerSyncer) findTargetService() (string, error) {
	id, err := ds.findServiceById(ds.target)
	if err != nil {
		return "", fmt.Errorf("failed to find service by ID or name %s: %w", ds.target, err)
	}
	if id != "" {
		return id, nil
	}
	return ds.findServiceByName(ds.target)
}

func (ds *DockerSyncer) getFirstRunningTaskForTargetService() (string, error) {
	tasks, err := ds.client.TaskList(context.Background(), types.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", ds.target),
			filters.Arg("desired-state", "running"),
		),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list tasks: %w", err)
	}
	if len(tasks) == 0 {
		return "", nil
	}
	return tasks[0].ID, nil
}

func (ds *DockerSyncer) getTaskContainerId(task string) (string, error) {
	taskInfo, _, err := ds.client.TaskInspectWithRaw(context.Background(), task)
	if err != nil {
		return "", fmt.Errorf("failed to inspect task %s: %w", task, err)
	}
	return taskInfo.Status.ContainerStatus.ContainerID, nil
}

func (ds *DockerSyncer) getContainerIdForTargetService() (string, error) {
	task, err := ds.getFirstRunningTaskForTargetService()
	if err != nil {
		return "", fmt.Errorf("failed to get first running task for service %s: %w", ds.target, err)
	}
	if task == "" {
		return "", nil
	}
	containerId, err := ds.getTaskContainerId(task)
	if err != nil {
		return "", fmt.Errorf("failed to get container ID for task %s: %w", task, err)
	}
	return containerId, nil
}

func (ds *DockerSyncer) restartTargetContainer(mountTemporaryVolume bool) error {
	ctx := context.Background()

	containerInfo, err := ds.client.ContainerInspect(ctx, ds.target)
	if err != nil {
		return fmt.Errorf("failed to inspect container %s: %w", ds.target, err)
	}

	timeout := stopTimeoutInSeconds
	err = ds.client.ContainerStop(ctx, ds.target, container.StopOptions{Timeout: &timeout})
	if err != nil {
		return fmt.Errorf("failed to stop container %s: %w", ds.target, err)
	}

	newConfig := containerInfo.Config
	newHostConfig := containerInfo.HostConfig

	mounts := []mount.Mount{}
	for _, mount := range newHostConfig.Mounts {
		if mount.Source != ds.temporaryVolume {
			mounts = append(mounts, mount)
		}
	}

	if mountTemporaryVolume {
		newMount := mount.Mount{
			Type:   mount.TypeVolume,
			Source: ds.temporaryVolume,
			Target: ds.targetPath,
		}
		newHostConfig.Mounts = append(mounts, newMount)
	} else {
		newHostConfig.Mounts = mounts
	}

	resp, err := ds.client.ContainerCreate(ctx, newConfig, newHostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create new container: %w", err)
	}

	err = ds.client.ContainerRemove(ctx, ds.target, container.RemoveOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove old container %s: %w", ds.target, err)
	}

	err = ds.client.ContainerStart(ctx, resp.ID, container.StartOptions{})
	if err != nil {
		return fmt.Errorf("failed to start new container: %w", err)
	}

	return nil
}

func (ds *DockerSyncer) restartTargetService(mountTemporaryVolume bool) error {
	serviceInfo, _, err := ds.client.ServiceInspectWithRaw(context.Background(), ds.target, types.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("failed to inspect service %s: %w", ds.target, err)
	}

	spec := serviceInfo.Spec
	spec.TaskTemplate.ForceUpdate++

	mounts := []mount.Mount{}
	hadTempVolume := false
	for _, mount := range spec.TaskTemplate.ContainerSpec.Mounts {
		if mount.Source == ds.temporaryVolume {
			hadTempVolume = true
		} else {
			mounts = append(mounts, mount)
		}
	}

	if mountTemporaryVolume {
		newMount := mount.Mount{
			Type:   mount.TypeVolume,
			Source: ds.temporaryVolume,
			Target: ds.targetPath,
		}
		spec.TaskTemplate.ContainerSpec.Mounts = append(mounts, newMount)
	} else {
		spec.TaskTemplate.ContainerSpec.Mounts = mounts
	}

	containerId := ""
	if hadTempVolume {
		containerId, _ = ds.getContainerIdForTargetService()
	}

	_, err = ds.client.ServiceUpdate(context.Background(), ds.target, serviceInfo.Version, spec, types.ServiceUpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update service %s: %w", ds.target, err)
	}

	if hadTempVolume && containerId != "" {
		ds.client.ContainerRemove(context.Background(), containerId, container.RemoveOptions{
			Force: true,
		})
	}

	return nil
}

func (ds *DockerSyncer) copyToContainer(sourcePath, container, containerPath string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to stat source: %w", err)
	}

	addToArchive := func(path string, info os.FileInfo, headerPath string) error {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("failed to create tar header: %w", err)
		}

		header.Name = headerPath

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header: %w", err)
		}

		if !info.IsDir() {
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("failed to open file: %w", err)
			}
			defer file.Close()

			if _, err := io.Copy(tw, file); err != nil {
				return fmt.Errorf("failed to copy file contents: %w", err)
			}
		}

		return nil
	}

	if sourceInfo.IsDir() {
		err = filepath.Walk(sourcePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return fmt.Errorf("failed to walk path %s: %w", sourcePath, err)
			}

			relPath, err := filepath.Rel(sourcePath, path)
			if err != nil {
				return fmt.Errorf("failed to get relative path: %w", err)
			}

			headerPath := filepath.Join(containerPath, relPath)
			headerPath = filepath.ToSlash(headerPath)

			return addToArchive(path, info, headerPath)
		})
	} else {
		headerPath := filepath.Join(containerPath, sourceInfo.Name())
		headerPath = filepath.ToSlash(headerPath)

		err = addToArchive(sourcePath, sourceInfo, headerPath)
	}

	if err != nil {
		return fmt.Errorf("failed to create tar archive: %w", err)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}

	err = ds.client.CopyToContainer(context.Background(), container, "/", &buf, types.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
	})
	if err != nil {
		return fmt.Errorf("failed to copy to container: %w", err)
	}

	return nil
}

func (ds *DockerSyncer) createTemporaryContainerWithVolume() error {
	vol, err := ds.client.VolumeCreate(context.Background(), volume.CreateOptions{
		Name: makeTemporaryName(),
		Labels: map[string]string{
			AppIdentifier: "true",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create volume: %w", err)
	}

	ds.temporaryVolume = vol.Name

	container, err := ds.client.ContainerCreate(context.Background(),
		&container.Config{
			Image: TemporaryContainerImage,
		},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: vol.Name,
					Target: TemporaryContainerMountPath,
				},
			},
			AutoRemove: true,
		},
		nil, nil, makeTemporaryName())
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	ds.temporaryContainer = container.ID

	return nil
}
