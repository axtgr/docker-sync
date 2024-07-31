package filewatcher

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	debounceInterval := 100 * time.Millisecond
	debounceTimers := make(map[string]*time.Timer)
	var mu sync.Mutex

	for {
		select {
		case event, ok := <-fw.Watcher.Events:
			if !ok {
				return
			}

			mu.Lock()
			if timer, exists := debounceTimers[event.Name]; exists {
				timer.Stop()
			}
			debounceTimers[event.Name] = time.AfterFunc(debounceInterval, func() {
				fw.processEvent(event)
				mu.Lock()
				delete(debounceTimers, event.Name)
				mu.Unlock()
			})
			mu.Unlock()

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

func (fw *FileWatcher) processEvent(event fsnotify.Event) {
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
