package cmd

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
)

func FindContainerById(needle string) (string, error) {
	command := exec.Command("docker", "ps", "--quiet", "-f", "id="+needle)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", errors.New(stderr.String())
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

func FindContainerByName(needle string) (string, error) {
	command := exec.Command("docker", "ps", "--quiet", "-f", "name="+needle)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", errors.New(stderr.String())
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

func FindContainer(idOrName string) (string, error) {
	id, err := FindContainerById(idOrName)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	name, err := FindContainerByName(idOrName)
	if err != nil {
		return "", err
	}
	if name != "" {
		return name, nil
	}
	return "", nil
}

func FindServiceById(needle string) (string, error) {
	command := exec.Command("docker", "service", "ls", "--quiet", "-f", "id="+needle)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", errors.New(stderr.String())
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

func FindServiceByName(needle string) (string, error) {
	command := exec.Command("docker", "service", "ls", "--quiet", "-f", "name="+needle)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", errors.New(stderr.String())
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

func FindService(idOrName string) (string, error) {
	id, err := FindServiceById(idOrName)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	name, err := FindServiceByName(idOrName)
	if err != nil {
		return "", err
	}
	if name != "" {
		return name, nil
	}
	return "", nil
}

func GetFirstRunningTaskForService(service string) (string, error) {
	command := exec.Command("docker", "service", "ps", "--quiet", "-f desired-state=running", service)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", errors.New(stderr.String())
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

func GetTaskContainerId(task string) (string, error) {
	command := exec.Command("docker", "inspect", "-f {{.Status.ContainerStatus.ContainerID}}", task)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", errors.New(stderr.String())
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

func GetContainerIdForService(service string) (string, error) {
	task, err := GetFirstRunningTaskForService(service)
	if err != nil {
		return "", err
	}
	if task == "" {
		return "", nil
	}

	container, err := GetTaskContainerId(task)
	if err != nil {
		return "", err
	}

	return container, nil
}

func RestartContainer(container string) error {
	command := exec.Command("docker", "restart", container)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		return errors.New(stderr.String())
	}
	return nil
}

func RestartService(service string, mount string) error {
	var command *exec.Cmd

	if mount == "" {
		command = exec.Command("docker", "service", "update", "--force", service)
	} else {
		command = exec.Command("docker", "service", "update", "--force", "--mount-add", mount, service)
	}

	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		return errors.New(stderr.String())
	}
	return nil
}

func CopyToContainer(sourcePath string, container string, containerPath string) error {
	cmd := exec.Command("docker", "cp", sourcePath, container+":"+containerPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return errors.New(stderr.String())
	}
	return nil
}

func CreateVolume() (string, error) {
	command := exec.Command("docker", "volume", "create")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", errors.New(stderr.String())
	}
	return strings.TrimSpace(string(output)), nil
}

var TemporaryContainerMountPath = "/docker-sync-data"

func CreateTemporaryContainerWithVolume() (string, string, error) {
	volume, err := CreateVolume()
	if err != nil {
		return "", "", err
	}

	command := exec.Command("docker", "create", "--quiet", "--rm", "--volume", volume+":"+TemporaryContainerMountPath, "hello-world")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", "", errors.New(stderr.String())
	}
	container := strings.TrimSpace(string(output))
	return container, volume, nil
}
