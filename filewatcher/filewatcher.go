package filewatcher

import (
	"log"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

type FileWatcher struct {
	Watcher *fsnotify.Watcher
	Events  chan fsnotify.Event
	Errors  chan error
	done    chan bool
}

const (
	Create = fsnotify.Create
	Write  = fsnotify.Write
	Remove = fsnotify.Remove
	Rename = fsnotify.Rename
)

func NewFileWatcher() (*FileWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	fw := &FileWatcher{
		Watcher: watcher,
		Events:  make(chan fsnotify.Event),
		Errors:  make(chan error),
		done:    make(chan bool),
	}

	go fw.Watch()

	return fw, nil
}

func (fw *FileWatcher) Watch() {
	for {
		select {
		case event, ok := <-fw.Watcher.Events:
			if !ok {
				return
			}

			// Remove events are reported on both dirs and files
			if event.Has(Remove) {
				fw.Events <- event
				return
			}

			fileInfo, err := os.Stat(event.Name)
			if err != nil {
				fw.Errors <- err
				return
			}

			// Events other than Remove are reported only on files
			if fileInfo.IsDir() {
				if event.Has(Create) {
					fw.AddWatch(event.Name)
				}
			} else if event.Has(Create) || event.Has(Write) || event.Has(Rename) {
				fw.Events <- event
			}
		case err, ok := <-fw.Watcher.Errors:
			if !ok {
				return
			}
			fw.Errors <- err
		case <-fw.done:
			return
		}
	}
}

func (fw *FileWatcher) AddWatch(path string) error {
	return filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			err = fw.Watcher.Add(path)
			if err != nil {
				log.Printf("Error watching path %s: %v\n", path, err)
			}
		}
		return nil
	})
}

func (fw *FileWatcher) Close() {
	close(fw.done)
	fw.Watcher.Close()
}
