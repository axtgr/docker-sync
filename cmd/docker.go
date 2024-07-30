package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
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

func isTemporaryVolumeMounted(spec *swarm.ServiceSpec) bool {
	for _, mount := range spec.TaskTemplate.ContainerSpec.Mounts {
		// Would be better to check for the label, but service specs don't include them
		if strings.HasPrefix(mount.Source, AppIdentifier) {
			return true
		}
	}
	return false
}

type DockerManager struct {
	client              *client.Client
	temporaryContainers []string
	temporaryVolumes    []string
}

func NewDockerManager(host ...string) (*DockerManager, error) {
	var dockerHost string

	if len(host) > 0 && host[0] != "" {
		dockerHost = host[0]
	} else {
		// Get the current Docker context
		cmd := exec.Command("docker", "context", "inspect")
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get Docker context: %w", err)
		}

		var contextInfo []struct {
			Name      string `json:"Name"`
			Endpoints struct {
				Docker struct {
					Host string `json:"Host"`
				} `json:"docker"`
			} `json:"Endpoints"`
		}
		if err := json.Unmarshal(output, &contextInfo); err != nil {
			return nil, fmt.Errorf("failed to parse Docker context: %w", err)
		}

		if len(contextInfo) == 0 {
			return nil, fmt.Errorf("no Docker context found")
		}

		dockerHost = contextInfo[0].Endpoints.Docker.Host
	}

	var clientOpts []client.Opt

	helper, err := connhelper.GetConnectionHelper(dockerHost)
	if err != nil {
		// Not an SSH URL, use default connection
		clientOpts = append(clientOpts, client.WithHost(dockerHost))
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
		)
	}

	// Check for DOCKER_API_VERSION environment variable
	version := os.Getenv("DOCKER_API_VERSION")
	if version != "" {
		clientOpts = append(clientOpts, client.WithVersion(version))
	} else {
		clientOpts = append(clientOpts, client.WithAPIVersionNegotiation())
	}

	// Create a new Docker client
	cli, err := client.NewClientWithOpts(clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &DockerManager{client: cli}, nil
}

func (dm *DockerManager) FindContainerById(needle string) (string, error) {
	containers, err := dm.client.ContainerList(context.Background(), container.ListOptions{
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

func (dm *DockerManager) FindContainerByName(needle string) (string, error) {
	containers, err := dm.client.ContainerList(context.Background(), container.ListOptions{
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

func (dm *DockerManager) FindContainer(idOrName string) (string, error) {
	id, err := dm.FindContainerById(idOrName)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	return dm.FindContainerByName(idOrName)
}

func (dm *DockerManager) FindServiceById(needle string) (string, error) {
	services, err := dm.client.ServiceList(context.Background(), types.ServiceListOptions{
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

func (dm *DockerManager) FindServiceByName(needle string) (string, error) {
	services, err := dm.client.ServiceList(context.Background(), types.ServiceListOptions{
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

func (dm *DockerManager) FindService(idOrName string) (string, error) {
	id, err := dm.FindServiceById(idOrName)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	return dm.FindServiceByName(idOrName)
}

func (dm *DockerManager) GetFirstRunningTaskForService(service string) (string, error) {
	tasks, err := dm.client.TaskList(context.Background(), types.TaskListOptions{
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

func (dm *DockerManager) GetTaskContainerId(task string) (string, error) {
	taskInfo, _, err := dm.client.TaskInspectWithRaw(context.Background(), task)
	if err != nil {
		return "", err
	}
	return taskInfo.Status.ContainerStatus.ContainerID, nil
}

func (dm *DockerManager) GetContainerIdForService(service string) (string, error) {
	task, err := dm.GetFirstRunningTaskForService(service)
	if err != nil {
		return "", err
	}
	if task == "" {
		return "", nil
	}
	return dm.GetTaskContainerId(task)
}

func (dm *DockerManager) RestartContainer(containerId string) error {
	timeout := DefaultRestartTimeout
	return dm.client.ContainerRestart(context.Background(), containerId, container.StopOptions{Timeout: &timeout})
}

func (dm *DockerManager) RestartService(service string, mountSource string, mountTarget string) error {
	serviceInfo, _, err := dm.client.ServiceInspectWithRaw(context.Background(), service, types.ServiceInspectOptions{})
	if err != nil {
		return err
	}

	spec := serviceInfo.Spec
	spec.TaskTemplate.ForceUpdate++

	if mountSource != "" && !isTemporaryVolumeMounted(&spec) {
		newMount := mount.Mount{
			Type:   mount.TypeVolume,
			Source: mountSource,
			Target: mountTarget,
		}
		spec.TaskTemplate.ContainerSpec.Mounts = append(spec.TaskTemplate.ContainerSpec.Mounts, newMount)
	}

	_, err = dm.client.ServiceUpdate(context.Background(), service, serviceInfo.Version, spec, types.ServiceUpdateOptions{})
	return err
}

func (dm *DockerManager) CopyToContainer(sourcePath, container, containerPath string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.Walk(sourcePath, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(fi, file)
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(sourcePath, file)
		if err != nil {
			return err
		}
		header.Name = filepath.Join(containerPath, relPath)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !fi.IsDir() {
			data, err := os.Open(file)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, data); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to create tar archive: %w", err)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}

	err = dm.client.CopyToContainer(context.Background(), container, "/", &buf, types.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
	})
	if err != nil {
		return fmt.Errorf("failed to copy to container: %w", err)
	}

	return nil
}

func (dm *DockerManager) CreateTemporaryContainerWithVolume() (string, string, error) {
	vol, err := dm.client.VolumeCreate(context.Background(), volume.CreateOptions{
		Name: makeTemporaryName(),
		Labels: map[string]string{
			AppIdentifier: "true",
		},
	})
	if err != nil {
		return "", "", err
	}

	dm.temporaryVolumes = append(dm.temporaryVolumes, vol.Name)

	container, err := dm.client.ContainerCreate(context.Background(),
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
		return "", "", err
	}

	dm.temporaryContainers = append(dm.temporaryContainers, container.ID)

	return container.ID, vol.Name, nil
}
