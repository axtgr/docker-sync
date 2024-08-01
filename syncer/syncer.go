package syncer

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
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
	TemporaryContainerImage = "hello-world"
	stopTimeoutInSeconds    = 10
)

type TargetType int

const (
	Container = iota
	Service
)

type Syncer struct {
	client             *client.Client
	host               string
	target             string
	targetType         TargetType
	targetPath         string
	restartTarget      bool
	temporaryContainer string
	temporaryVolume    string
	logger             *log.Logger
	identifier         string
}

type Options struct {
	Target        string
	TargetPath    string
	RestartTarget bool
	Host          string
	Logger        *log.Logger
	Identifier    string
}

func New(options Options) (*Syncer, error) {
	return &Syncer{
		host:          options.Host,
		target:        options.Target,
		targetPath:    options.TargetPath,
		restartTarget: options.RestartTarget,
		logger:        options.Logger,
		identifier:    options.Identifier,
	}, nil
}

func (syncer *Syncer) generateTemporaryName() string {
	return syncer.identifier + "-" + uuid.New().String()
}

func (syncer *Syncer) getTemporaryVolumePath() string {
	return "/" + syncer.identifier + "-data"
}

func (syncer *Syncer) Connect() error {
	var clientOpts []client.Opt

	helper, err := connhelper.GetConnectionHelper(syncer.host)
	if err != nil {
		// Not an SSH URL, use default connection
		clientOpts = append(clientOpts, client.WithHost(syncer.host))
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

	syncer.client = client
	return nil
}

func (syncer *Syncer) Init() error {
	err := syncer.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to docker: %w", err)
	}

	service, err := syncer.findTargetService()
	if err != nil {
		return fmt.Errorf("failed to find service %s: %w", syncer.target, err)
	}

	if service == "" {
		container, err := syncer.findTargetContainer()
		if err != nil {
			return fmt.Errorf("failed to find container %s: %w", syncer.target, err)
		}
		if container == "" {
			return fmt.Errorf("failed to find container or service %s", syncer.target)
		}

		syncer.targetType = Container
		syncer.target = container
	} else {
		syncer.targetType = Service
		syncer.target = service
	}

	if syncer.restartTarget && syncer.targetType == Service {
		err := syncer.createTemporaryContainerWithVolume()
		if err != nil {
			return fmt.Errorf("failed to create a temporary container with a volume: %w", err)
		}
	}

	return nil
}

func (syncer *Syncer) Copy(localPath string, op filewatcher.Op) error {
	if syncer.targetType == Container && !syncer.restartTarget {
		container, err := syncer.findTargetContainer()
		if err != nil {
			return fmt.Errorf("failed to find container %s: %w", syncer.target, err)
		}

		err = syncer.copyToContainer(localPath, container, syncer.targetPath)
		if err != nil {
			return fmt.Errorf("failed to copy to container %s: %w", container, err)
		}
	} else if syncer.targetType == Container && syncer.restartTarget {
		container, err := syncer.findTargetContainer()
		if err != nil {
			return fmt.Errorf("failed to find container %s: %w", syncer.target, err)
		}

		err = syncer.copyToContainer(localPath, container, syncer.targetPath)
		if err != nil {
			return fmt.Errorf("failed to copy to container %s: %w", container, err)
		}

		err = syncer.recreateTargetContainer(true)
		if err != nil {
			return fmt.Errorf("failed to restart container %s: %w", container, err)
		}
	} else if syncer.targetType == Service && !syncer.restartTarget {
		container, err := syncer.getContainerIdForTargetService()
		if err != nil {
			return fmt.Errorf("failed to container ID for service %s: %w", syncer.target, err)
		}

		err = syncer.copyToContainer(localPath, container, syncer.targetPath)
		if err != nil {
			return fmt.Errorf("failed to copy to container %s: %w", container, err)
		}
	} else if syncer.targetType == Service && syncer.restartTarget {
		err := syncer.copyToContainer(localPath, syncer.temporaryContainer, syncer.getTemporaryVolumePath())
		if err != nil {
			return fmt.Errorf("failed to copy to temporary container %s: %w", syncer.temporaryContainer, err)
		}

		err = syncer.updateTargetService(true)
		if err != nil {
			return fmt.Errorf("failed to restart service %s: %w", syncer.target, err)
		}
	}

	return nil
}

func (syncer *Syncer) Cleanup() error {
	syncer.logger.Println("Cleaning up...")

	ctx := context.Background()

	if syncer.targetType == Container {
		syncer.logger.Printf("Recreating container %s...", syncer.target)
		err := syncer.recreateTargetContainer(false)
		if err != nil {
			return fmt.Errorf("failed to restart target container %s: %w", syncer.target, err)
		}
	} else {
		syncer.logger.Printf("Updating service %s...", syncer.target)
		err := syncer.updateTargetService(false)
		if err != nil {
			return fmt.Errorf("failed to restart target service: %w", err)
		}
	}

	syncer.logger.Printf("Removing temporary container %s...", syncer.temporaryContainer)
	err := syncer.client.ContainerRemove(ctx, syncer.temporaryContainer, container.RemoveOptions{
		Force: true,
	})
	if err != nil {
		return fmt.Errorf("failed to remove temporary container %s: %w", syncer.temporaryContainer, err)
	}

	syncer.logger.Printf("Removing temporary volume %s...", syncer.temporaryVolume)
	err = syncer.client.VolumeRemove(ctx, syncer.temporaryVolume, true)
	if err != nil {
		return fmt.Errorf("failed to remove temporary volume %s: %w", syncer.temporaryVolume, err)
	}

	syncer.temporaryContainer = ""
	syncer.temporaryVolume = ""

	return nil
}

func (syncer *Syncer) findContainerById(needle string) (string, error) {
	containers, err := syncer.client.ContainerList(context.Background(), container.ListOptions{
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

func (syncer *Syncer) findContainerByName(needle string) (string, error) {
	containers, err := syncer.client.ContainerList(context.Background(), container.ListOptions{
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

func (syncer *Syncer) findTargetContainer() (string, error) {
	id, err := syncer.findContainerById(syncer.target)
	if err != nil {
		return "", fmt.Errorf("failed to find container by ID or name %s: %w", syncer.target, err)
	}
	if id != "" {
		return id, nil
	}
	containerId, err := syncer.findContainerByName(syncer.target)
	if err != nil {
		return "", fmt.Errorf("failed to find container by ID or name %s: %w", syncer.target, err)
	}
	return containerId, nil
}

func (syncer *Syncer) findServiceById(needle string) (string, error) {
	services, err := syncer.client.ServiceList(context.Background(), types.ServiceListOptions{
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

func (syncer *Syncer) findServiceByName(needle string) (string, error) {
	services, err := syncer.client.ServiceList(context.Background(), types.ServiceListOptions{
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

func (syncer *Syncer) findTargetService() (string, error) {
	id, err := syncer.findServiceById(syncer.target)
	if err != nil {
		return "", fmt.Errorf("failed to find service by ID or name %s: %w", syncer.target, err)
	}
	if id != "" {
		return id, nil
	}
	return syncer.findServiceByName(syncer.target)
}

func (syncer *Syncer) getFirstRunningTaskForTargetService() (string, error) {
	tasks, err := syncer.client.TaskList(context.Background(), types.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", syncer.target),
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

func (syncer *Syncer) getTaskContainerId(task string) (string, error) {
	taskInfo, _, err := syncer.client.TaskInspectWithRaw(context.Background(), task)
	if err != nil {
		return "", fmt.Errorf("failed to inspect task %s: %w", task, err)
	}
	return taskInfo.Status.ContainerStatus.ContainerID, nil
}

func (syncer *Syncer) getContainerIdForTargetService() (string, error) {
	task, err := syncer.getFirstRunningTaskForTargetService()
	if err != nil {
		return "", fmt.Errorf("failed to get first running task for service %s: %w", syncer.target, err)
	}
	if task == "" {
		return "", nil
	}
	containerId, err := syncer.getTaskContainerId(task)
	if err != nil {
		return "", fmt.Errorf("failed to get container ID for task %s: %w", task, err)
	}
	return containerId, nil
}

func (syncer *Syncer) recreateTargetContainer(mountTemporaryVolume bool) error {
	ctx := context.Background()

	containerInfo, err := syncer.client.ContainerInspect(ctx, syncer.target)
	if err != nil {
		return fmt.Errorf("failed to inspect container %s: %w", syncer.target, err)
	}

	syncer.logger.Printf("Stopping container %s...", syncer.target)
	timeout := stopTimeoutInSeconds
	err = syncer.client.ContainerStop(ctx, syncer.target, container.StopOptions{Timeout: &timeout})
	if err != nil {
		return fmt.Errorf("failed to stop container %s: %w", syncer.target, err)
	}

	newConfig := containerInfo.Config
	newHostConfig := containerInfo.HostConfig

	mounts := []mount.Mount{}
	for _, mount := range newHostConfig.Mounts {
		if mount.Source != syncer.temporaryVolume {
			mounts = append(mounts, mount)
		}
	}

	if mountTemporaryVolume {
		syncer.logger.Println("Creating a container with a temporary volume...")
		newMount := mount.Mount{
			Type:   mount.TypeVolume,
			Source: syncer.temporaryVolume,
			Target: syncer.targetPath,
		}
		newHostConfig.Mounts = append(mounts, newMount)
	} else {
		syncer.logger.Println("Creating a container without temporary volumes...")
		newHostConfig.Mounts = mounts
	}

	newTarget, err := syncer.client.ContainerCreate(ctx, newConfig, newHostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create new container: %w", err)
	}
	syncer.target = newTarget.ID

	syncer.logger.Println("Removing the old container...", syncer.target)
	err = syncer.client.ContainerRemove(ctx, syncer.target, container.RemoveOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove old container %s: %w", syncer.target, err)
	}

	syncer.logger.Println("Starting the new container...", syncer.target)
	err = syncer.client.ContainerStart(ctx, newTarget.ID, container.StartOptions{})
	if err != nil {
		return fmt.Errorf("failed to start new container: %w", err)
	}

	return nil
}

func (syncer *Syncer) updateTargetService(mountTemporaryVolume bool) error {
	serviceInfo, _, err := syncer.client.ServiceInspectWithRaw(context.Background(), syncer.target, types.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("failed to inspect service %s: %w", syncer.target, err)
	}

	spec := serviceInfo.Spec
	spec.TaskTemplate.ForceUpdate++

	mounts := []mount.Mount{}
	hadTempVolume := false
	for _, mount := range spec.TaskTemplate.ContainerSpec.Mounts {
		if mount.Source == syncer.temporaryVolume {
			hadTempVolume = true
		} else {
			mounts = append(mounts, mount)
		}
	}

	if mountTemporaryVolume {
		syncer.logger.Printf("Updating service %s with temporary volume...\n", syncer.target)
		newMount := mount.Mount{
			Type:   mount.TypeVolume,
			Source: syncer.temporaryVolume,
			Target: syncer.targetPath,
		}
		spec.TaskTemplate.ContainerSpec.Mounts = append(mounts, newMount)
	} else {
		syncer.logger.Printf("Updating service %s without temporary volume...\n", syncer.target)
		spec.TaskTemplate.ContainerSpec.Mounts = mounts
	}

	containerId := ""
	if hadTempVolume {
		containerId, _ = syncer.getContainerIdForTargetService()
	}

	_, err = syncer.client.ServiceUpdate(context.Background(), syncer.target, serviceInfo.Version, spec, types.ServiceUpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update service %s: %w", syncer.target, err)
	}

	if hadTempVolume && containerId != "" {
		syncer.logger.Printf("Removing old container %s for service %s...\n", containerId, syncer.target)
		syncer.client.ContainerRemove(context.Background(), containerId, container.RemoveOptions{
			Force: true,
		})
	}

	return nil
}

func (syncer *Syncer) copyToContainer(sourcePath, container, containerPath string) error {
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

	err = syncer.client.CopyToContainer(context.Background(), container, "/", &buf, types.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
	})
	if err != nil {
		return fmt.Errorf("failed to copy to container: %w", err)
	}

	return nil
}

func (syncer *Syncer) createTemporaryContainerWithVolume() error {
	volumeName := syncer.generateTemporaryName()
	syncer.logger.Printf("Creating temporary volume %s...\n", volumeName)
	vol, err := syncer.client.VolumeCreate(context.Background(), volume.CreateOptions{
		Name: volumeName,
		Labels: map[string]string{
			syncer.identifier: "true",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create volume: %w", err)
	}

	syncer.temporaryVolume = vol.Name

	containerName := syncer.generateTemporaryName()
	syncer.logger.Printf("Creating temporary container %s...\n", containerName)
	container, err := syncer.client.ContainerCreate(context.Background(),
		&container.Config{
			Image: TemporaryContainerImage,
		},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: vol.Name,
					Target: syncer.getTemporaryVolumePath(),
				},
			},
			AutoRemove: true,
		},
		nil, nil, containerName)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	syncer.temporaryContainer = container.ID

	return nil
}
