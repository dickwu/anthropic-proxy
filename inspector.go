package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type requestSnapshot struct {
	Method  string      `json:"method"`
	URI     string      `json:"uri"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}
type snapshotStore struct {
	mu     sync.RWMutex
	latest requestSnapshot
}

func (s *snapshotStore) set(value requestSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest = value
}
func (s *snapshotStore) get() requestSnapshot { s.mu.RLock(); defer s.mu.RUnlock(); return s.latest }

func startInspector(path string, snapshots *snapshotStore) (net.Listener, error) {
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("inspector path is not a socket")
		}
		connection, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			connection.Close()
			return nil, fmt.Errorf("inspector is already running")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connection.SetDeadline(time.Now().Add(5 * time.Second))
			json.NewEncoder(connection).Encode(snapshots.get())
			connection.Close()
		}
	}()
	return listener, nil
}

func ensurePrivateDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("directory must already be private (0700): %s", directory)
	}
	return nil
}
