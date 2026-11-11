package main

// `memory import`: the CLI side and the daemon's /import endpoint. The daemon does the work in
// the background (memdimport.go); the command starts it and follows its progress.
//
//	POST /import[?dry_run=1]   202 + status JSON; 409 + status JSON when a run is in progress
//	GET  /import               200 + status JSON of the current or last run

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const importPrintEvery = 5 * time.Second

// importPollEvery is how often the command asks the daemon for progress (a var for tests).
var importPollEvery = time.Second

func registerImport(mux *http.ServeMux) {
	reply := func(w http.ResponseWriter, code int, st importStatus) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(st)
	}
	mux.HandleFunc("POST /import", func(w http.ResponseWriter, r *http.Request) {
		imp := memdActiveImporter.Load()
		if imp == nil {
			writeText(w, 503, "Error: import is not available")
			return
		}
		if err := imp.Start(imp.ctx, r.URL.Query().Get("dry_run") == "1"); err != nil {
			reply(w, http.StatusConflict, imp.Status())
			return
		}
		reply(w, http.StatusAccepted, imp.Status())
	})
	mux.HandleFunc("GET /import", func(w http.ResponseWriter, r *http.Request) {
		imp := memdActiveImporter.Load()
		if imp == nil {
			writeText(w, 503, "Error: import is not available")
			return
		}
		reply(w, http.StatusOK, imp.Status())
	})
}

func importBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func importFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// importLine renders one status line.
func importLine(st importStatus) string {
	switch {
	case st.DryRun && !st.Running:
		return fmt.Sprintf("memory import: dry run, %s (%s) to read; %d already imported, %d waiting for the tailer",
			importFiles(st.FilesTotal), importBytes(st.BytesTotal), st.FilesCurrent, st.FilesDeferred)
	case st.Running:
		return fmt.Sprintf("memory import: %d/%d files, %s / %s read, %d rows added", st.FilesDone, st.FilesTotal,
			importBytes(st.BytesDone), importBytes(st.BytesTotal), st.Rows)
	}
	return fmt.Sprintf("memory import: done, %d/%d files, %s read, %d rows added in %v; %d already imported, %d waiting for the tailer, %d failed",
		st.FilesDone, st.FilesTotal, importBytes(st.BytesDone), st.Rows, st.Finished.Sub(st.Started).Round(time.Second),
		st.FilesCurrent, st.FilesDeferred, st.FilesFailed)
}

// memImportTo is `memory import [--dry-run] [--status]`: it asks the daemon on sock to start
// an import and prints progress until it ends. Stopping the command does not stop the import.
func memImportTo(args []string, sock string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("memory import", flag.ContinueOnError)
	fs.SetOutput(errw)
	dry := fs.Bool("dry-run", false, "count what would be read, write nothing")
	status := fs.Bool("status", false, "show the current or last import and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(errw, "memory import: %v (is the daemon running? it starts with a Claude Code session)\n", err)
		return 1
	}
	decode := func(code int, body []byte) (importStatus, error) {
		var st importStatus
		if code != http.StatusOK && code != http.StatusAccepted && code != http.StatusConflict {
			return st, fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(string(body), "Error: ")))
		}
		return st, json.Unmarshal(body, &st)
	}
	if *status {
		code, body, err := memGetStatus(sock, "/import", 10*time.Second)
		if err != nil {
			return fail(err)
		}
		st, err := decode(code, body)
		if err != nil {
			fmt.Fprintf(errw, "memory import: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, importLine(st))
		return 0
	}

	path := "/import"
	if *dry {
		path += "?dry_run=1"
	}
	code, body, err := memRequest(http.MethodPost, sock, path, 10*time.Second)
	if err != nil {
		return fail(err)
	}
	st, err := decode(code, body)
	if err != nil {
		fmt.Fprintf(errw, "memory import: %v\n", err)
		return 1
	}
	if code == http.StatusConflict {
		fmt.Fprintf(errw, "memory import: an import is already running\n%s\n", importLine(st))
		return 1
	}
	if !*dry {
		fmt.Fprintln(out, "memory import: started in the daemon; this prints progress, and stopping the command does not stop the import")
	}
	last := time.Time{}
	for {
		code, body, err := memGetStatus(sock, "/import", 10*time.Second)
		if err != nil {
			return fail(err)
		}
		if st, err = decode(code, body); err != nil {
			fmt.Fprintf(errw, "memory import: %v\n", err)
			return 1
		}
		if !st.Running {
			break
		}
		if time.Since(last) >= importPrintEvery {
			fmt.Fprintln(out, importLine(st))
			last = time.Now()
		}
		time.Sleep(importPollEvery)
	}
	fmt.Fprintln(out, importLine(st))
	if st.Err != "" {
		fmt.Fprintf(errw, "memory import: %s\n", st.Err)
		return 1
	}
	return 0
}
