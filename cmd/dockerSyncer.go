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
	"strings"

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
	DefaultRestartTimeout       = 10
)

func makeTemporaryName() string {
	return AppIdentifier + "-" + uuid.New().String()
}

func isTemporaryVolume(mount *mount.Mount) bool {
	return strings.HasPrefix(mount.Source, AppIdentifier)
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
		return err
	}

	service, err := ds.findService(ds.target)
	if err != nil {
		return err
	}

	if service == "" {
		ds.targetType = Container
	} else {
		ds.targetType = Service
	}

	if ds.restartTarget && ds.targetType == Service {
		err := ds.createTemporaryContainerWithVolume()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	}

	return nil
}

func (ds *DockerSyncer) Sync(localPath string, op filewatcher.Op) error {
	if ds.targetType == Container && !ds.restartTarget {
		container, err := ds.findContainer(ds.target)
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
		container, err := ds.findContainer(ds.target)
		if err != nil {
			return fmt.Errorf("failed to find container %s: %w", ds.target, err)
		}

		err = ds.copyToContainer(localPath, container, ds.targetPath)
		if err != nil {
			return fmt.Errorf("failed to copy to container %s: %w", container, err)
		}

		err = ds.restartContainer(container)
		if err != nil {
			return fmt.Errorf("failed to restart container %s: %w", container, err)
		}

		return nil
	}

	if ds.targetType == Service && !ds.restartTarget {
		container, err := ds.getContainerIdForService(ds.target)
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

		err = ds.restartService(ds.target, ds.temporaryVolume, ds.targetPath)
		if err != nil {
			return fmt.Errorf("failed to restart service %s: %w", ds.target, err)
		}
	}

	return nil
}

func (ds *DockerSyncer) Cleanup() error {
	ctx := context.Background()

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
		return "", err
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
		return "", err
	}
	if len(containers) == 0 {
		return "", nil
	}
	return containers[0].ID, nil
}

func (ds *DockerSyncer) findContainer(idOrName string) (string, error) {
	id, err := ds.findContainerById(idOrName)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	return ds.findContainerByName(idOrName)
}

func (ds *DockerSyncer) findServiceById(needle string) (string, error) {
	services, err := ds.client.ServiceList(context.Background(), types.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("id", needle)),
	})
	if err != nil {
		return "", err
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
		return "", err
	}
	if len(services) == 0 {
		return "", nil
	}
	return services[0].ID, nil
}

func (ds *DockerSyncer) findService(idOrName string) (string, error) {
	id, err := ds.findServiceById(idOrName)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	return ds.findServiceByName(idOrName)
}

func (ds *DockerSyncer) getFirstRunningTaskForService(service string) (string, error) {
	tasks, err := ds.client.TaskList(context.Background(), types.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", service),
			filters.Arg("desired-state", "running"),
		),
	})
	if err != nil {
		return "", err
	}
	if len(tasks) == 0 {
		return "", nil
	}
	return tasks[0].ID, nil
}

func (ds *DockerSyncer) getTaskContainerId(task string) (string, error) {
	taskInfo, _, err := ds.client.TaskInspectWithRaw(context.Background(), task)
	if err != nil {
		return "", err
	}
	return taskInfo.Status.ContainerStatus.ContainerID, nil
}

func (ds *DockerSyncer) getContainerIdForService(service string) (string, error) {
	task, err := ds.getFirstRunningTaskForService(service)
	if err != nil {
		return "", err
	}
	if task == "" {
		return "", nil
	}
	return ds.getTaskContainerId(task)
}

func (ds *DockerSyncer) restartContainer(containerId string) error {
	timeout := DefaultRestartTimeout
	return ds.client.ContainerRestart(context.Background(), containerId, container.StopOptions{Timeout: &timeout})
}

func (ds *DockerSyncer) restartService(service string, mountSource string, mountTarget string) error {
	serviceInfo, _, err := ds.client.ServiceInspectWithRaw(context.Background(), service, types.ServiceInspectOptions{})
	if err != nil {
		return err
	}

	spec := serviceInfo.Spec
	spec.TaskTemplate.ForceUpdate++

	if mountSource != "" {
		newMount := mount.Mount{
			Type:   mount.TypeVolume,
			Source: mountSource,
			Target: mountTarget,
		}
		mounts := []mount.Mount{}
		for _, mount := range spec.TaskTemplate.ContainerSpec.Mounts {
			if !isTemporaryVolume(&mount) {
				mounts = append(mounts, mount)
			}
		}
		spec.TaskTemplate.ContainerSpec.Mounts = append(mounts, newMount)
	}

	_, err = ds.client.ServiceUpdate(context.Background(), service, serviceInfo.Version, spec, types.ServiceUpdateOptions{})
	return err
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
				return err
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
		return err
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
		return err
	}

	ds.temporaryContainer = container.ID

	return nil
}
