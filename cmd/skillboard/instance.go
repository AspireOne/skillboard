package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// One window at a time. A launch for the same scopes focuses the open window; a launch
// for other scopes starts its own server and moves the open window over to it.
//
// The instance file only says where to look. Whether that server still runs, and what
// it shows, is asked over HTTP, so a stale file is harmless.

const (
	instanceFile = "skillboard-instance.json"
	// The server answers from localhost in well under this, or is gone.
	instanceTimeout = time.Second
)

type instance struct {
	URL string `json:"url"`
	Key string `json:"key"` // which build shows which scopes; equal keys mean the same window
}

// instanceKey includes the binary's mtime, so a freshly installed build takes the window
// over instead of focusing one still served by the old build.
func instanceKey(root string, projects []string) string {
	build := "unknown"
	if exe, err := os.Executable(); err == nil {
		if info, err := os.Stat(exe); err == nil {
			build = info.ModTime().UTC().Format(time.RFC3339Nano)
		}
	}
	return strings.Join(append([]string{build, root}, projects...), "\n")
}

// runningInstance returns the open instance, or nil if none answers.
func runningInstance() *instance {
	dir, err := stateDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, instanceFile))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("reading instance file failed", "err", err)
		}
		return nil
	}
	var saved instance
	if err := json.Unmarshal(data, &saved); err != nil {
		slog.Warn("instance file unreadable", "err", err)
		return nil
	}
	client := http.Client{Timeout: instanceTimeout}
	res, err := client.Get(saved.URL + "/api/instance")
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	var live instance
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&live) != nil {
		return nil
	}
	live.URL = saved.URL
	return &live
}

func saveInstance(inst instance) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	data, err := json.Marshal(inst)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, instanceFile), data, 0o644)
}

// moveWindows asks the running instance to send its windows to url, and reports how
// many it had.
func moveWindows(from *instance, url string) (int, error) {
	body, err := json.Marshal(map[string]string{"url": url})
	if err != nil {
		return 0, err
	}
	client := http.Client{Timeout: instanceTimeout}
	res, err := client.Post(from.URL+"/api/instance/move", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	var out struct {
		Windows int `json:"windows"`
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("moving windows: %s", res.Status)
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Windows, nil
}
