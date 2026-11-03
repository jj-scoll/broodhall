package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	memdMaxBody      = 1 << 20 // request body cap for POST /tool/{name}
	byFileMaxObs     = 3
	byFileMaxBytes   = 1500
	socketDialBudget = 500 * time.Millisecond
)

// memdTools are the MCP tools served over POST /tool/{name}: the eleven memory tools minus smart_*.
var memdTools = map[string]func(db *sql.DB, args map[string]any) (string, error){
	"search":           memSearch,
	"timeline":         memTimeline,
	"get_observations": memGetObservations,
	"get_tool_uses":    memGetToolUses,
	"work_state_read":  memWorkStateRead,
	"work_state_write": memWorkStateWrite,
	"save_memory":      memSaveMemory,
}

// memdHandler serves the daemon's socket API:
//
//	GET  /healthz                      200 "ok"
//	GET  /context?project=P[&source=S] 200, exactly contextJSON's bytes (SessionStart hook output);
//	                                   claims the project's unshown handoff (not for source=resume)
//	GET  /by-file?path=F[&project=P]   200, <= 3 observations touching F, <= 1500 bytes; empty body if none
//	POST /tool/{name}                  JSON object of arguments in, tool text out
//
// POST /tool status mapping, which the MCP server maps to isError: 200 is a normal result;
// 400 is a tool-reported failure (text beginning "Error:" or "Invalid", returned by the tool
// with a nil error; bad JSON; missing project for session_start_context) with the text as the
// body; 404 is an unknown tool; 405 a wrong method; 413 an oversize body; 500 a Go error
// from the tool (body is the error message).
func memdHandler(db *sql.DB) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, 200, "ok")
	})
	mux.HandleFunc("GET /context", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		if project == "" {
			writeText(w, 400, "Error: project is required")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write(contextJSON(buildStartContext(db, project, r.URL.Query().Get("source"), time.Now())))
	})
	mux.HandleFunc("GET /by-file", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeText(w, 400, "Error: path is required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), memdByFileDeadline)
		defer cancel()
		body, err := byFileCtx(ctx, db, path, r.URL.Query().Get("project"))
		if err != nil {
			writeText(w, 500, err.Error())
			return
		}
		writeText(w, 200, body)
	})
	mux.HandleFunc("POST /tool/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		fn, ok := memdTools[name]
		if !ok && name != "session_start_context" {
			writeText(w, 404, "Error: unknown tool "+name)
			return
		}
		args, status, msg := readArgs(w, r)
		if status != 0 {
			writeText(w, status, msg)
			return
		}
		var out string
		var err error
		if name == "session_start_context" {
			project := startContextProject(args)
			if project == "" {
				writeText(w, 400, `session_start_context: "project" or "projects" is required`)
				return
			}
			out = buildContext(db, project, time.Now())
		} else {
			out, err = fn(db, args)
		}
		switch {
		case err != nil:
			writeText(w, 500, err.Error())
		case strings.HasPrefix(out, "Error:") || strings.HasPrefix(out, "Invalid"):
			writeText(w, 400, out)
		default:
			writeText(w, 200, out)
		}
	})
	mux.HandleFunc("/tool/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		writeText(w, 405, "Error: method not allowed")
	})
	return mux
}

func writeText(w http.ResponseWriter, code int, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	io.WriteString(w, s)
}

// readArgs decodes the JSON argument object; an empty body means no arguments.
func readArgs(w http.ResponseWriter, r *http.Request) (map[string]any, int, string) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, memdMaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, http.StatusRequestEntityTooLarge, "Error: request body too large"
		}
		return nil, 400, "Error: reading body: " + err.Error()
	}
	args := map[string]any{}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return args, 0, ""
	}
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return nil, 400, "Error: body must be a JSON object"
	}
	return args, 0, ""
}

// startContextProject follows the session_start_context arguments: "projects" (array or
// comma-separated string) or "project". The last project is primary; buildContext renders one
// project, so the last entry is used.
func startContextProject(args map[string]any) string {
	if ps := qList(args, "projects"); len(ps) > 0 {
		return strings.TrimSpace(ps[len(ps)-1])
	}
	if s, ok := args["projects"].(string); ok {
		parts := strings.Split(s, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if p := strings.TrimSpace(parts[i]); p != "" {
				return p
			}
		}
	}
	return strings.TrimSpace(qStr(args, "project"))
}

// byFile lists the newest observations whose observation_files path equals path or ends in
// "/"+path (the matching searchObs uses for path queries), cut to byFileMaxBytes on a rune boundary.
func byFile(db *sql.DB, path, project string) (string, error) {
	return byFileCtx(context.Background(), db, path, project)
}

// byFileCtx is byFile under ctx; the query is cancelled with it.
func byFileCtx(ctx context.Context, db *sql.DB, path, project string) (string, error) {
	p := strings.TrimPrefix(path, "./")
	q := `SELECT ` + obsCols + ` FROM observations
		WHERE id IN (SELECT observation_id FROM observation_files WHERE path = ? OR path LIKE ? ESCAPE '\')`
	args := []any{p, "%/" + likeEscape(p)}
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, byFileMaxObs)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return "", fmt.Errorf("by-file: %w", err)
	}
	defer rows.Close()
	obs, err := scanObs(rows)
	if err != nil {
		return "", fmt.Errorf("by-file: %w", err)
	}
	var b strings.Builder
	for _, o := range obs {
		fmt.Fprintf(&b, "#%d %s %s %s", o.id, fmtDay(o.created), memdTypeIcon(o.typ), ctxFlat(o.title))
		if o.subtitle != "" {
			b.WriteString(" - " + ctxFlat(o.subtitle))
		}
		b.WriteByte('\n')
	}
	s := b.String()
	if len(s) > byFileMaxBytes {
		n := byFileMaxBytes
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s, nil
}

// listenSocket listens on a unix socket at path with mode 0600 from creation (umask 0177
// around bind; the umask is process-wide, so call it at daemon start, not concurrently with
// file creation). A leftover socket is removed only when dialing it fails; a live listener
// yields an error and its socket is left alone.
func listenSocket(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, socketDialBudget); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: already in use by a running daemon", path)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
}
