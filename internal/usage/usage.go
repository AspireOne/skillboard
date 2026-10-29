// Package usage counts how often each skill is used, from the local session
// transcripts of Claude Code, Codex and Pi.
//
// A skill counts once per session that used it. Re-reading a SKILL.md later in the
// same session is not a new use. Results are cached per transcript, so only new or
// grown transcripts are read again.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	Claude = "claude"
	Codex  = "codex"
	Pi     = "pi"
)

// Bump when a parser changes, so every transcript is read again.
const cacheVersion = 1

// Use is one session that used a skill.
type Use struct {
	Skill   string `json:"skill"`
	Harness string `json:"harness"`
	Day     string `json:"day"`    // local date of the first use, YYYY-MM-DD
	Manual  bool   `json:"manual"` // the user invoked it, rather than the model
}

// A hit is one sighting of a skill on a transcript line.
type hit struct {
	skill  string
	manual bool
}

type source struct {
	harness string
	dirs    []string
	// Lines without any of these bytes are skipped without parsing.
	markers [][]byte
	parse   func(line []byte) (timestamp string, hits []hit)
}

func sources(home string) []source {
	return []source{
		{Claude, []string{filepath.Join(home, ".claude", "projects")}, [][]byte{[]byte(`"Skill"`), []byte("<command-name>")}, parseClaude},
		{Codex, []string{filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".codex", "archived_sessions")}, [][]byte{[]byte("SKILL.md")}, parseCodex},
		{Pi, []string{filepath.Join(home, ".pi", "agent", "sessions")}, [][]byte{[]byte("SKILL.md"), []byte("<skill name=")}, parsePi},
	}
}

var (
	// A read of a skill's instructions, e.g. `cat ~/.agents/skills/x/SKILL.md`.
	skillFile = regexp.MustCompile(`skills/([a-z0-9][a-z0-9-]*)/SKILL\.md`)
	// Claude Code records a typed slash command, built-in or skill, like this.
	claudeCommand = regexp.MustCompile(`<command-name>/([^<\s]+)</command-name>`)
	// Codex injects a skill the user mentioned with $name like this.
	codexMention = regexp.MustCompile(`^<skill>\n<name>([^<]+)</name>`)
	// Pi expands /skill:name like this.
	piCommand = regexp.MustCompile(`^<skill name="([^"]+)"`)
)

func readHits(text string, manual bool) []hit {
	var hits []hit
	for _, m := range skillFile.FindAllStringSubmatch(text, -1) {
		hits = append(hits, hit{m[1], manual})
	}
	return hits
}

// Claude Code: the model calls the Skill tool; the user types /name.
func parseClaude(line []byte) (string, []hit) {
	var rec struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return "", nil
	}
	var hits []hit
	switch rec.Type {
	case "assistant":
		var parts []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Skill string `json:"skill"`
			} `json:"input"`
		}
		json.Unmarshal(rec.Message.Content, &parts) //nolint:errcheck // other shapes carry no tool calls
		for _, p := range parts {
			if p.Type == "tool_use" && p.Name == "Skill" && p.Input.Skill != "" {
				hits = append(hits, hit{p.Input.Skill, false})
			}
		}
	case "user":
		var text string
		if json.Unmarshal(rec.Message.Content, &text) == nil {
			for _, m := range claudeCommand.FindAllStringSubmatch(text, -1) {
				hits = append(hits, hit{m[1], true})
			}
		}
	}
	return rec.Timestamp, hits
}

// Codex: the model reads SKILL.md with a tool call; the user mentions $name.
func parseCodex(line []byte) (string, []hit) {
	var rec struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "response_item" {
		return "", nil
	}
	switch rec.Payload.Type {
	case "function_call", "custom_tool_call", "local_shell_call":
		// The line holds only the call, so any SKILL.md path on it is what the call reads.
		return rec.Timestamp, readHits(string(line), false)
	case "message":
		if rec.Payload.Role != "user" {
			return "", nil
		}
		var hits []hit
		for _, c := range rec.Payload.Content {
			if m := codexMention.FindStringSubmatch(c.Text); m != nil {
				hits = append(hits, hit{m[1], true})
			}
		}
		return rec.Timestamp, hits
	}
	return "", nil
}

// Pi: the model reads SKILL.md with a tool call; the user types /skill:name.
func parsePi(line []byte) (string, []hit) {
	var rec struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "message" {
		return "", nil
	}
	var hits []hit
	for _, c := range rec.Message.Content {
		switch {
		case rec.Message.Role == "assistant" && c.Type == "toolCall":
			hits = append(hits, readHits(string(c.Arguments), false)...)
		case rec.Message.Role == "user" && c.Type == "text":
			if m := piCommand.FindStringSubmatch(c.Text); m != nil {
				hits = append(hits, hit{m[1], true})
			}
		}
	}
	return rec.Timestamp, hits
}

// scanFile reads one transcript into one Use per skill.
func scanFile(path string, src source) ([]Use, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	uses := []Use{}
	index := map[string]int{}
	err = eachLine(f, func(line []byte) {
		if !containsAny(line, src.markers) {
			return
		}
		ts, hits := src.parse(line)
		for _, h := range hits {
			if i, ok := index[h.skill]; ok {
				uses[i].Manual = uses[i].Manual || h.manual
				continue
			}
			index[h.skill] = len(uses)
			uses = append(uses, Use{Skill: h.skill, Harness: src.harness, Day: localDay(ts), Manual: h.manual})
		}
	})
	return uses, err
}

func containsAny(line []byte, markers [][]byte) bool {
	for _, m := range markers {
		if bytes.Contains(line, m) {
			return true
		}
	}
	return false
}

// eachLine streams lines of any length; transcripts hold lines of several megabytes.
func eachLine(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var long []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long = append(long, chunk...)
			continue
		}
		line := chunk
		if long != nil {
			line = append(long, chunk...)
			long = nil
		}
		if len(line) > 0 {
			fn(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func localDay(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ""
	}
	return t.Local().Format(time.DateOnly)
}

type cacheEntry struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"modTime"`
	Uses    []Use `json:"uses"`
}

type cacheFile struct {
	Version int                    `json:"version"`
	Files   map[string]*cacheEntry `json:"files"`
}

// Scan returns every use found in the transcripts under home, updating the cache at
// cachePath.
//
// Entries are keyed by harness and file name rather than full path: Codex moves
// transcripts into archived_sessions, and the session ID in the name is unique.
// Entries whose transcript is gone are kept, since Claude Code deletes transcripts
// after 30 days by default and their usage is history worth keeping.
func Scan(home, cachePath string) ([]Use, error) {
	start := time.Now()
	cache := loadCache(cachePath)

	type job struct {
		key, path string
		src       source
		info      fs.FileInfo
	}
	var jobs []job
	seen := map[string]bool{}
	for _, src := range sources(home) {
		for _, dir := range src.dirs {
			err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil // harness not installed
					}
					return err
				}
				if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				key := src.harness + "/" + d.Name()
				if seen[key] {
					return nil // the same session in both live and archived folders
				}
				seen[key] = true
				if e := cache.Files[key]; e != nil && e.Size == info.Size() && e.ModTime == info.ModTime().UnixMicro() {
					return nil
				}
				jobs = append(jobs, job{key, path, src, info})
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}

	var mu sync.Mutex
	var firstErr error
	work := make(chan job)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for j := range work {
				uses, err := scanFile(j.path, j.src)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				cache.Files[j.key] = &cacheEntry{Size: j.info.Size(), ModTime: j.info.ModTime().UnixMicro(), Uses: uses}
				mu.Unlock()
			}
		})
	}
	for _, j := range jobs {
		work <- j
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	if len(jobs) > 0 {
		if err := saveCache(cachePath, cache); err != nil {
			slog.Warn("saving usage cache failed", "err", err)
		}
	}
	uses := []Use{}
	for _, e := range cache.Files {
		uses = append(uses, e.Uses...)
	}
	slog.Info("scanned skill usage", "transcripts", len(cache.Files), "read", len(jobs), "uses", len(uses), "took", time.Since(start))
	return uses, nil
}

func loadCache(path string) *cacheFile {
	empty := &cacheFile{Version: cacheVersion, Files: map[string]*cacheEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("reading usage cache failed, rescanning", "err", err)
		}
		return empty
	}
	var c cacheFile
	if err := json.Unmarshal(data, &c); err != nil || c.Version != cacheVersion || c.Files == nil {
		slog.Info("usage cache unusable, rescanning", "err", err)
		return empty
	}
	return &c
}

// saveCache writes through a temporary file, so a concurrent launch never reads half a cache.
func saveCache(path string, c *cacheFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".usage-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
