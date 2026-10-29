// Command skillboard opens a local web UI for browsing and editing ~/.agents, plus the
// project-local AGENTS.md and .agents/skills of the project it is launched from.
//
// By default it detaches: the terminal returns at once, the UI opens in a browser app
// window, and the background process exits when that window closes.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"skillboard/internal/harness"
	"skillboard/internal/launch"
	"skillboard/internal/lint"
	"skillboard/internal/usage"
	"skillboard/internal/workspace"
	"skillboard/web"
)

const (
	// Set on the detached child; the listener the parent opened arrives as fd 3.
	childEnv = "SKILLBOARD_CHILD"
	// Rendered Markdown may contain raw HTML; only our own scripts may run, since this
	// origin can write files.
	// Under the user cache directory, beside the browser profile.
	usageCache = "Skillboard/usage.json"
	contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("cannot find home directory", err)
	}
	root := flag.String("root", filepath.Join(home, ".agents"), "agents directory to manage")
	addr := flag.String("addr", "127.0.0.1:0", "listen address (port 0 picks a free one)")
	foreground := flag.Bool("foreground", false, "serve in this terminal instead of detaching")
	from := flag.String("project", "", "where to look for project AGENTS.md and .agents, walking up to the repository root (default: the working directory)")
	open := flag.Bool("open", true, "open the app window")
	dotfiles := flag.String("dotfiles", filepath.Join(home, ".cfg"), "bare git repository of your dotfiles, with home as its work tree; skills it ignores are shown as machine-local")
	flag.Parse()

	// Absolute and clean, so the project walk can compare directories exactly.
	if *root, err = filepath.Abs(*root); err != nil {
		fatal("invalid -root", err)
	}
	if info, err := os.Stat(*root); err != nil || !info.IsDir() {
		fatal("agents directory not found: "+*root, err)
	}
	if *from, err = filepath.Abs(*from); err != nil { // "" resolves to the working directory
		fatal("invalid -project", err)
	}
	if info, err := os.Stat(*from); err != nil || !info.IsDir() {
		fatal("project directory not found: "+*from, err)
	}
	projects := workspace.FindProjects(*from, home, *root)
	var scopes []scope
	for i, dir := range projects {
		id := fmt.Sprintf("project-%d", i+1)
		scopes = append(scopes, scope{workspace.Project(id, dir), harness.Project(id, dir)})
	}
	global := workspace.Global(*root)
	if info, err := os.Stat(*dotfiles); err == nil && info.IsDir() {
		global.Dotfiles = []string{"--git-dir", *dotfiles, "--work-tree", home}
	} else {
		slog.Info("no dotfiles repository, so no skill is marked machine-local", "dotfiles", *dotfiles)
	}
	scopes = append(scopes, scope{global, harness.Global(home, *root)})
	key := instanceKey(*root, projects)

	if os.Getenv(childEnv) != "" {
		ln, err := net.FileListener(os.NewFile(3, "listener"))
		if err != nil {
			fatal("cannot use inherited listener", err)
		}
		serve(ln, scopes, home, key)
		return
	}

	// Only launches that open a window take part, so development servers never touch it.
	var running *instance
	if *open {
		running = runningInstance()
		if running != nil && running.Key == key {
			focusOrOpen(running.URL)
			fmt.Printf("Skillboard: already open at %s\n", running.URL)
			return
		}
	}

	// Listening before detaching surfaces port errors here, in the terminal.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fatal("cannot listen", err)
	}
	url := "http://" + ln.Addr().String()

	if !*foreground {
		if err := detach(ln); err != nil {
			fatal("cannot start background process", err)
		}
	}
	if *open {
		openWindow(running, url)
		if err := saveInstance(instance{URL: url, Key: key}); err != nil {
			slog.Warn("saving instance file failed", "err", err)
		}
	}
	if *foreground {
		slog.Info("serving", "url", url, "root", *root, "projects", projects)
		serve(ln, scopes, home, key)
		return
	}
	if len(projects) > 0 {
		fmt.Printf("Skillboard: %s with %s (closes with its window)\n", url, strings.Join(projects, ", "))
	} else {
		fmt.Printf("Skillboard: %s (closes with its window)\n", url)
	}
}

// openWindow moves the running instance's window to url, or opens a new one.
func openWindow(running *instance, url string) {
	if running != nil {
		windows, err := moveWindows(running, url)
		if err != nil {
			slog.Warn("could not take over the open window", "err", err)
		}
		if windows > 0 {
			if err := launch.Focus(); err != nil && !errors.Is(err, errors.ErrUnsupported) {
				slog.Warn("could not focus the window", "err", err)
			}
			return
		}
	}
	if err := launch.OpenApp(url); err != nil {
		slog.Warn("could not open a window", "err", err)
	}
}

// focusOrOpen brings the window forward, or opens one where that is not possible.
func focusOrOpen(url string) {
	err := launch.Focus()
	if err == nil {
		return
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		slog.Warn("could not focus the window, opening another", "err", err)
	}
	if err := launch.OpenApp(url); err != nil {
		slog.Warn("could not open a window", "err", err)
	}
}

// detach re-runs this binary in its own session, handing over the open listener.
func detach(ln net.Listener) error {
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := openLog()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.ExtraFiles = []*os.File{f}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// openLog truncates the log on every launch, so it only ever holds the latest session.
func openLog() (*os.File, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	return os.Create(filepath.Join(dir, "skillboard.log"))
}

func stateDir() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return dir, os.MkdirAll(dir, 0o755)
}

// scope is one workspace with its harness links. Project scopes come first, nearest
// first, then the global one.
type scope struct {
	ws    workspace.Workspace
	links harness.Set
}

func findScope(scopes []scope, id string) (scope, error) {
	for _, s := range scopes {
		if s.ws.ID == id {
			return s, nil
		}
	}
	return scope{}, fmt.Errorf("scope %q: %w", id, fs.ErrNotExist)
}

// scopeOfPath picks the scope by a path's first segment, its workspace ID.
func scopeOfPath(scopes []scope, path string) (scope, error) {
	id, _, _ := strings.Cut(path, "/")
	return findScope(scopes, id)
}

func serve(ln net.Listener, scopes []scope, home, key string) {
	life := newLifetime()
	// The first scan reads gigabytes of transcripts, so it starts before the window asks.
	scanUsage := sync.OnceValues(func() ([]usage.Use, error) {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		return usage.Scan(home, filepath.Join(cache, usageCache))
	})
	go scanUsage() //nolint:errcheck // the result is served by /api/usage
	go func() {
		if err := http.Serve(ln, routes(scopes, life, scanUsage, key)); err != nil {
			fatal("server stopped", err)
		}
	}()
	<-life.Done()
	slog.Info("window closed, exiting")
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

func routes(scopes []scope, life *lifetime, scanUsage func() ([]usage.Use, error), key string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(web.Static()))

	// The window holds this stream open for as long as it lives.
	mux.HandleFunc("GET /api/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, ": connected\n\n")
		http.NewResponseController(w).Flush() //nolint:errcheck // a dead client shows up as ctx.Done below
		life.Connect()
		defer life.Disconnect()
		select {
		case <-r.Context().Done():
		case <-life.Moved():
			// The window saves, then leaves; its disconnect ends this instance.
			fmt.Fprintf(w, "event: move\ndata: %s\n\n", life.MovedTo())
			http.NewResponseController(w).Flush() //nolint:errcheck // a dead client shows up as ctx.Done below
			<-r.Context().Done()
		}
	})

	mux.HandleFunc("GET /api/instance", func(w http.ResponseWriter, r *http.Request) {
		respond(w, instance{Key: key}, nil)
	})

	// A newer launch for other scopes takes this instance's windows over.
	mux.HandleFunc("POST /api/instance/move", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL string `json:"url"`
		}
		if !decode(w, r, &req) {
			return
		}
		if !strings.HasPrefix(req.URL, "http://127.0.0.1:") {
			http.Error(w, "only a local Skillboard can take the windows over", http.StatusBadRequest)
			return
		}
		slog.Info("moving windows to a newer instance", "url", req.URL)
		respond(w, map[string]int{"windows": life.MoveTo(req.URL)}, nil)
	})

	mux.HandleFunc("GET /api/tree", func(w http.ResponseWriter, r *http.Request) {
		trees := []workspace.Tree{}
		for _, s := range scopes {
			tree, err := s.ws.Tree()
			if err != nil {
				respond(w, nil, err)
				return
			}
			trees = append(trees, tree)
		}
		workspace.MarkShadowed(trees)
		respond(w, trees, nil)
	})

	mux.HandleFunc("GET /api/harnesses", func(w http.ResponseWriter, r *http.Request) {
		checks := []harness.Check{}
		for _, s := range scopes {
			checks = append(checks, s.links.Checks()...)
		}
		respond(w, checks, nil)
	})

	mux.HandleFunc("POST /api/harnesses", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope string `json:"scope"`
			Path  string `json:"path"`
		}
		if !decode(w, r, &req) {
			return
		}
		s, err := findScope(scopes, req.Scope)
		if err == nil {
			err = s.links.Create(req.Path)
		}
		respond(w, map[string]bool{"ok": true}, err)
	})

	// Scanned once per launch: transcripts change slowly compared to a window's life.
	mux.HandleFunc("GET /api/usage", func(w http.ResponseWriter, r *http.Request) {
		uses, err := scanUsage()
		respond(w, uses, err)
	})

	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		hits := []workspace.SearchHit{}
		for _, s := range scopes {
			found, err := s.ws.Search(r.URL.Query().Get("q"))
			if err != nil {
				respond(w, nil, err)
				return
			}
			hits = append(hits, found...)
		}
		respond(w, hits, nil)
	})

	mux.HandleFunc("GET /api/file", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		s, err := scopeOfPath(scopes, path)
		if err != nil {
			respond(w, nil, err)
			return
		}
		content, file, err := s.ws.Read(path)
		respond(w, map[string]any{"content": content, "file": file}, err)
	})

	mux.HandleFunc("PUT /api/file", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Content   string `json:"content"`
			BaseMtime *int64 `json:"baseMtime"`
		}
		if !decode(w, r, &req) {
			return
		}
		path := r.URL.Query().Get("path")
		s, err := scopeOfPath(scopes, path)
		if err != nil {
			respond(w, nil, err)
			return
		}
		file, err := s.ws.Write(path, req.Content, req.BaseMtime)
		respond(w, file, err)
	})

	mux.HandleFunc("POST /api/skills", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope string `json:"scope"`
			Name  string `json:"name"`
		}
		if !decode(w, r, &req) {
			return
		}
		s, err := findScope(scopes, req.Scope)
		if err != nil {
			respond(w, nil, err)
			return
		}
		file, err := s.ws.CreateSkill(req.Name)
		if err == nil {
			linkSkills(s)
		}
		respond(w, file, err)
	})

	mux.HandleFunc("DELETE /api/skills", func(w http.ResponseWriter, r *http.Request) {
		dir := r.URL.Query().Get("dir")
		s, err := scopeOfPath(scopes, dir)
		if err == nil {
			err = s.ws.DeleteSkill(dir)
		}
		respond(w, map[string]bool{"ok": true}, err)
	})

	mux.HandleFunc("POST /api/skills/fix", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Dir     string `json:"dir"`
			Message string `json:"message"`
		}
		if !decode(w, r, &req) {
			return
		}
		s, err := scopeOfPath(scopes, req.Dir)
		if err == nil {
			err = s.ws.FixSkill(req.Dir, req.Message)
		}
		respond(w, map[string]bool{"ok": true}, err)
	})

	mux.HandleFunc("POST /api/skills/{scope}/{name}/enabled", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if !decode(w, r, &req) {
			return
		}
		s, err := findScope(scopes, r.PathValue("scope"))
		if err != nil {
			respond(w, nil, err)
			return
		}
		dir, err := s.ws.SetEnabled(r.PathValue("name"), req.Enabled)
		if err == nil {
			linkSkills(s)
		}
		respond(w, map[string]string{"dir": dir}, err)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		mux.ServeHTTP(w, r)
	})
}

// linkSkills runs after anything that may have created the skills directory: the first
// project skill creates .agents/skills, which Claude Code only sees through its link. The
// skill change itself succeeded, so a failed link is logged and shows up in the overview.
func linkSkills(s scope) {
	if err := s.links.LinkSkills(); err != nil {
		slog.Error("creating skills link failed", "scope", s.ws.ID, "err", err)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, workspace.ErrConflict), errors.Is(err, workspace.ErrExists), errors.Is(err, fs.ErrExist):
			status = http.StatusConflict
		case errors.Is(err, lint.ErrNotFixed):
			status = http.StatusUnprocessableEntity
		case errors.Is(err, workspace.ErrNotText):
			status = http.StatusUnsupportedMediaType
		case errors.Is(err, workspace.ErrInvalidName), errors.Is(err, workspace.ErrOutsideRoot):
			status = http.StatusBadRequest
		case errors.Is(err, fs.ErrNotExist):
			status = http.StatusNotFound
		}
		if status == http.StatusInternalServerError {
			slog.Error("request failed", "err", err)
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encoding response failed", "err", err)
	}
}
