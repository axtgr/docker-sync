package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/axtgr/docker-sync/filewatcher"
	"github.com/axtgr/docker-sync/syncer"
	"github.com/spf13/cobra"
)

const (
	ColorReset = "\033[0m"
	ColorBlue  = "\033[34m"
)

var rootCmd = &cobra.Command{
	Use:   "docker-sync <source> <destination>",
	Short: "Sync files with a remote Docker container/service",
	Long:  "Watch a local directory and sync its contents with a remote Docker container or service",
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

		destinationTarget := destinationSegments[0]
		destinationPath := destinationSegments[1]

		verbose, err := cmd.Flags().GetBool("verbose")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}

		var verboseLogger *log.Logger
		if verbose {
			verboseLogger = log.New(os.Stdout, "", 0)
		} else {
			verboseLogger = log.New(io.Discard, "", 0)
		}

		restart, err := cmd.Flags().GetBool("restart")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}

		dockerHost, err := cmd.Flags().GetString("host")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}

		if dockerHost == "" {
			cmd := exec.Command("docker", "context", "inspect")
			output, err := cmd.Output()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(1)
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
				err = fmt.Errorf("failed to parse Docker context: %w", err)
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(1)
				return
			}

			if len(contextInfo) == 0 {
				fmt.Fprintln(os.Stderr, "Error: no Docker context found")
				os.Exit(1)
			}

			dockerHost = contextInfo[0].Endpoints.Docker.Host
		}

		dockerSyncer, err := syncer.New(syncer.Options{
			Target:        destinationTarget,
			TargetPath:    destinationPath,
			RestartTarget: restart,
			Host:          dockerHost,
			Logger:        verboseLogger,
			Identifier:    "docker-sync",
		})

		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		err = dockerSyncer.Connect()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		err = dockerSyncer.Init()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		defer dockerSyncer.Cleanup()

		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

		go func() {
			<-signals
			err := dockerSyncer.Cleanup()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error while cleaning up:", err)
			}
			os.Exit(0)
		}()

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

		fmt.Printf("Syncing %s%s%s to %s%s%s\n", ColorBlue, absoluteSourcePath, ColorReset, ColorBlue, destination, ColorReset)

		for {
			select {
			case event := <-fw.Events:
				if event.Has(filewatcher.Create) || event.Has(filewatcher.Write) {
					fmt.Printf("Copying %s to %s...\n", event.Name, destinationPath)
					err := dockerSyncer.Copy(event.Name, event.Op)
					fmt.Printf("Copied %s to %s\n", event.Name, destinationPath)
					if err != nil {
						fmt.Fprintln(os.Stderr, "Error:", err)
					}
				}
			case err := <-fw.Errors:
				fmt.Fprintln(os.Stderr, "Error:", err)
			}
		}
	},
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().BoolP("restart", "r", false, "Restart container/service on changes")
	rootCmd.Flags().Bool("verbose", false, "Log every interaction with Docker")
	rootCmd.Flags().StringP("host", "H", "", "Docker host to use")
}
