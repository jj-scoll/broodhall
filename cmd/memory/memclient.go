package main

// Thin client side of memory: memGet over the unix socket, the bounded file lookup the
// guard hooks use, and `memory context` (the SessionStart hook).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	memLookupBudget  = 150 * time.Millisecond
	memContextBudget = 2 * time.Second
	memLookupMaxText = 1500 // bytes; matches /by-file's own cap
	memLookupMinSize = 1500 // files smaller than this are not worth an injection
	memLookupMaxBody = 64 << 10
)

// memdSockPath is the daemon's socket: $XDG_RUNTIME_DIR/memory.sock.
func memdSockPath() string {
	return filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "memory.sock")
}

// memGet does GET path (request URI, query included) on the unix socket sock. One deadline of
// budget covers dial, request and read. Any status but 200 is an error.
func memGet(sock, path string, budget time.Duration) ([]byte, error) {
	status, body, err := memGetStatus(sock, path, budget)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("memory: %d %s", status, http.StatusText(status))
	}
	return body, nil
}

// memGetStatus is memGet without the status check: it returns the status and body of any
// response, and an error only when the daemon could not be reached or read.
func memGetStatus(sock, path string, budget time.Duration) (int, []byte, error) {
	return memRequest(http.MethodGet, sock, path, budget)
}

// memRequest is memGetStatus for any method (the handover posts to /shutdown).
func memRequest(method, sock, path string, budget time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cl := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, method, "http://memory"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// memdProjectKey is projectKey; a var so tests can stand in a slow one.
var memdProjectKey = projectKey

// memdLookup asks the daemon for what memory knows about path. 150 ms for dial, request and
// read; any error gives "". A var so tests never need a daemon.
var memdLookup = func(path, project string) string {
	q := "/by-file?path=" + url.QueryEscape(path) + "&project=" + url.QueryEscape(project)
	b, err := memGet(memdSockPath(), q, memLookupBudget)
	if err != nil {
		return ""
	}
	return string(b)
}

// memdProjectDir is the directory the project name comes from: CLAUDE_PROJECT_DIR is preferred
// to over the hook's cwd.
func memdProjectDir(cwd string) string {
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
		return d
	}
	return cwd
}

// memdLookupFile returns the memory text for file, or "". It never blocks the hook past
// memLookupBudget (projectKey included: a cold one forks git), and skips subagent calls and
// files that are missing, not regular or under memLookupMinSize.
func memdLookupFile(cwd, file string, subagent bool) string {
	if subagent || file == "" {
		return ""
	}
	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.Mode().IsRegular() || st.Size() < memLookupMinSize {
		return ""
	}
	res := make(chan string, 1)
	// Read the vars here: a goroutine that outlives the budget must not touch them later.
	lookup, pk, dir := memdLookup, memdProjectKey, memdProjectDir(cwd)
	go func() { res <- lookup(abs, pk(dir)) }()
	t := time.NewTimer(memLookupBudget)
	defer t.Stop()
	select {
	case s := <-res:
		return capText(strings.TrimSpace(s), memLookupMaxText)
	case <-t.C:
		return ""
	}
}

// capText cuts s to at most n bytes without splitting a rune.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// memdShellLookup reports whether Bash/Grep/Glob lookups are on (default off).
func memdShellLookup() bool { return os.Getenv("MEMORY_LOOKUP_SHELL") == "1" }

// shellFileCandidates returns the path-like arguments of cat/sed/head/tail/rg/less in cmd.
func shellFileCandidates(cmd string) []string {
	cmds, ok := parseShell(cmd)
	if !ok {
		return nil
	}
	var out []string
	for _, c := range cmds {
		if len(c.Words) < 2 {
			continue
		}
		switch filepath.Base(c.Words[0]) {
		case "cat", "sed", "head", "tail", "rg", "less":
		default:
			continue
		}
		for _, w := range c.Words[1:] {
			if w != "" && !strings.HasPrefix(w, "-") {
				out = append(out, w)
			}
		}
	}
	return out
}

// memdShellFile picks the file a Bash/Grep/Glob call touches: Grep/Glob `path`, or the first
// candidate in a Bash command that is a regular file. "" when there is none.
func memdShellFile(tool string, input map[string]any, cwd string) string {
	var cands []string
	switch tool {
	case "Grep", "Glob":
		if p, _ := input["path"].(string); p != "" {
			cands = []string{p}
		}
	case "Bash":
		c, _ := input["command"].(string)
		cands = shellFileCandidates(c)
	}
	for _, p := range cands {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, abs)
		}
		if st, err := os.Stat(abs); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// memdToolLookup is the lookup for one allowed tool call: Edit/Write/NotebookEdit by
// file_path (notebook_path), and, only with MEMORY_LOOKUP_SHELL=1, Bash/Grep/Glob.
func memdToolLookup(tool string, input map[string]any, cwd string, subagent bool) string {
	if subagent {
		return ""
	}
	var file string
	switch tool {
	case "Edit", "Write": // Read is read-dedup's (R37)
		file, _ = input["file_path"].(string)
	case "NotebookEdit":
		file, _ = input["notebook_path"].(string)
		if file == "" {
			file, _ = input["file_path"].(string)
		}
	case "Bash", "Grep", "Glob":
		if !memdShellLookup() {
			return ""
		}
		file = memdShellFile(tool, input, cwd)
	}
	return memdLookupFile(cwd, file, false)
}

// memContext is `memory context`: the SessionStart hook. Any error prints an empty
// block (valid hook JSON, no context) and exits 0.
func memContext(args []string) int {
	empty := func() int {
		fmt.Println(`{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":""}}`)
		return 0
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return empty()
	}
	var in struct {
		Cwd       string `json:"cwd"`
		Source    string `json:"source"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(data, &in) != nil {
		return empty()
	}
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	sock, pk, dir := memdSockPath(), memdProjectKey, memdProjectDir(in.Cwd)
	go func() {
		q := "/context?project=" + url.QueryEscape(pk(dir)) + "&cwd=" + url.QueryEscape(dir)
		if in.Source != "" {
			q += "&source=" + url.QueryEscape(in.Source)
		}
		if in.SessionID != "" {
			q += "&session_id=" + url.QueryEscape(in.SessionID)
		}
		b, err := memGet(sock, q, memContextBudget)
		ch <- res{b, err}
	}()
	t := time.NewTimer(memContextBudget)
	defer t.Stop()
	select {
	case r := <-ch:
		if r.err != nil || len(r.b) == 0 {
			return empty()
		}
		os.Stdout.Write(r.b)
		return 0
	case <-t.C:
		return empty()
	}
}
