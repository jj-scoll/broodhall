package main

// record-edit (PostToolUse hook on Write, Edit, MultiEdit and NotebookEdit): appends the
// edited file's real path to <session>.edits in the handoff state dir, one "epoch<TAB>quoted
// path" line per edit. The sweep guard (sweepguard.go) reads these files to tell whose
// uncommitted change a `git add -A` would take in. Best effort and cheap: one small append,
// no git, no output, and it never fails the tool call. Edits made through Bash (sed -i,
// redirects) are not seen.

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// sweepWindow is how recently a session must have shown life to count as live.
const sweepWindow = 30 * time.Minute

func editsPath(session string) string { return filepath.Join(stateDir(), session+".edits") }

// editTools maps each recording tool to the tool_input key that holds its path.
var editTools = map[string]string{"Write": "file_path", "Edit": "file_path", "MultiEdit": "file_path", "NotebookEdit": "notebook_path"}

func recordEdit(args []string) int {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 0
	}
	var in struct {
		SessionID string         `json:"session_id"`
		Cwd       string         `json:"cwd"`
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
	}
	if json.Unmarshal(data, &in) != nil || !safeSession.MatchString(in.SessionID) {
		return 0
	}
	key, ok := editTools[in.ToolName]
	if !ok {
		return 0
	}
	p, _ := in.ToolInput[key].(string)
	if p == "" {
		return 0
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(in.Cwd, p)
	}
	appendEdit(in.SessionID, realPath(p), time.Now())
	return 0
}

// realPath cleans p and resolves symlinks in its directory, so a path reached through a
// symlink matches the toplevel git reports. The file itself is left alone: it may be gone.
func realPath(p string) string {
	p = filepath.Clean(p)
	if d, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(d, filepath.Base(p))
	}
	return p
}

func appendEdit(session, path string, now time.Time) {
	f := editsPath(session)
	if os.MkdirAll(filepath.Dir(f), 0o755) != nil {
		return
	}
	if st, err := os.Stat(f); err == nil && st.Size() > 64<<10 {
		pruneEdits(f, now)
	}
	fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	fh.WriteString(strconv.FormatInt(now.Unix(), 10) + "\t" + strconv.Quote(path) + "\n")
}

// pruneEdits keeps one line per path and drops those older than a day.
func pruneEdits(f string, now time.Time) {
	var b strings.Builder
	for p, e := range readEdits(f) {
		if now.Sub(time.Unix(e, 0)) <= 24*time.Hour {
			b.WriteString(strconv.FormatInt(e, 10) + "\t" + strconv.Quote(p) + "\n")
		}
	}
	os.WriteFile(f, []byte(b.String()), 0o644)
}

// readEdits returns each recorded path with the epoch of its latest edit.
func readEdits(f string) map[string]int64 {
	fh, err := os.Open(f)
	if err != nil {
		return nil
	}
	defer fh.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		es, q, ok := strings.Cut(sc.Text(), "\t")
		e, err := strconv.ParseInt(es, 10, 64)
		p, err2 := strconv.Unquote(q)
		if ok && err == nil && err2 == nil {
			out[p] = e
		}
	}
	return out
}

// otherSession is another session that recorded edits and showed life within the window.
type otherSession struct {
	ID    string
	Last  time.Time
	Files map[string]bool // absolute paths it edited
}

// liveOtherSessions lists the sessions other than self that recorded edits and are live.
// Liveness signals, all existing state: the mtime of the session's own edits file (touched by
// every recorded edit), its guard state and guard ledger (touched when a guard marks or logs),
// and the read-dedup ledger in /tmp (touched by every Read). The newest one counts.
func liveOtherSessions(self string, now time.Time, window time.Duration) []otherSession {
	entries, _ := os.ReadDir(stateDir())
	var out []otherSession
	for _, de := range entries {
		id, isEdits := strings.CutSuffix(de.Name(), ".edits")
		if !isEdits || id == self || !safeSession.MatchString(id) {
			continue
		}
		m := filepath.Join(stateDir(), de.Name())
		var last time.Time
		for _, p := range []string{m, filepath.Join(stateDir(), id+".guard"), guardLedger(id), readCachePath(id)} {
			if st, err := os.Stat(p); err == nil && st.ModTime().After(last) {
				last = st.ModTime()
			}
		}
		if last.IsZero() || now.Sub(last) > window {
			continue
		}
		s := otherSession{ID: id, Last: last, Files: map[string]bool{}}
		for p := range readEdits(m) {
			s.Files[p] = true
		}
		if len(s.Files) > 0 {
			out = append(out, s)
		}
	}
	return out
}
