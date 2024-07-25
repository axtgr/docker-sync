package cmd

import (
	"fmt"
	"os"
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

		destinationContainerOrService := destinationSegments[0]
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
					service, err := FindService(destinationContainerOrService)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error while finding service %s\n", destinationContainerOrService)
						fmt.Fprintln(os.Stderr, err)
						return
					}

					var container string

					if service == "" {
						container, err = FindContainer(destinationContainerOrService)
						if err != nil {
							fmt.Fprintf(os.Stderr, "Error while finding container %s\n", destinationContainerOrService)
							fmt.Fprintln(os.Stderr, err)
							return
						}
					} else {
						container, err = GetContainerIdForService(destinationContainerOrService)
						if err != nil {
							fmt.Fprintf(os.Stderr, "Error while getting container ID for service %s\n", destinationContainerOrService)
							fmt.Fprintln(os.Stderr, err)
							return
						}
					}

					err = CopyToContainer(event.Name, container, destinationPath)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error while copying %s to %s:%s\n", event.Name, destinationContainerOrService, destinationPath)
						fmt.Fprintln(os.Stderr, err)
						return
					}

					if restart && service != "" {
						fmt.Printf("Restarting service %s\n", destinationContainerOrService)
						err := RestartService(service)
						if err != nil {
							fmt.Fprintf(os.Stderr, "Error while restarting service %s\n", destinationContainerOrService)
							fmt.Fprintln(os.Stderr, err)
							return
						}
					} else {
						fmt.Printf("Restarting container %s\n", destinationContainerOrService)
						err := RestartContainer(container)
						if err != nil {
							fmt.Fprintf(os.Stderr, "Error while restarting container %s\n", destinationContainerOrService)
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

// Adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().BoolP("restart", "r", false, "Restart container/service after syncing")
}