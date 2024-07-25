package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/axtgr/docker-sync/filewatcher"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "docker-sync <source> <destination>",
	Short: "Sync files with a remote Docker container",
	Long:  `Watch a local directory and sync it with a remote Docker container using docker cp.`,
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		absoluteSourcePath, err := filepath.Abs(args[0])

		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}

		destination := args[1]
		destinationSegments := strings.Split(destination, ":")

		if len(destinationSegments) < 2 || destinationSegments[0] == "" || destinationSegments[1] == "" {
			fmt.Fprintln(os.Stderr, "Destination must be in the following format: <container>:<path>")
			os.Exit(1)
		}

		destinationContainer := destinationSegments[0]
		destinationPath := destinationSegments[1]

		restart, err := cmd.Flags().GetBool("restart")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}

		fmt.Printf("Syncing %s to %s\n", absoluteSourcePath, destination)

		fw, err := filewatcher.NewFileWatcher()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		defer fw.Close()

		err = fw.AddWatch(absoluteSourcePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}

		for {
			select {
			case event := <-fw.Events:
				if event.Has(filewatcher.Create) || event.Has(filewatcher.Write) {
					err := copyToContainer(event.Name, destinationContainer, destinationPath)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error while copying %s to %s:%s\n", event.Name, destinationContainer, destinationPath)
						fmt.Fprintln(os.Stderr, err)
						return
					}

					if restart {
						fmt.Printf("Restarting container %s\n", destinationContainer)
						err := restartContainer(destinationContainer)
						if err != nil {
							fmt.Fprintf(os.Stderr, "Error while restarting container %s\n", destinationContainer)
							fmt.Fprintln(os.Stderr, err)
							return
						}
					}
				}
			case err := <-fw.Errors:
				fmt.Fprintln(os.Stderr, "Error:", err)
			}
		}
	},
}

func copyToContainer(sourcePath string, container string, containerPath string) error {
	cmd := exec.Command("docker", "cp", sourcePath, container+":"+containerPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return errors.New(stderr.String())
	}
	return nil
}

func restartContainer(container string) error {
	cmd := exec.Command("docker", "restart", container)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return errors.New(stderr.String())
	}
	return nil
}

// Adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().BoolP("restart", "r", false, "Restart container after syncing")
}
