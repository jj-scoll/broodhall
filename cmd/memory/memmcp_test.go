package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// mcpRun feeds lines to memMCP and returns the decoded response objects.
func mcpRun(t *testing.T, sock string, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if rc := memMCP(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, sock); rc != 0 {
		t.Fatalf("memMCP rc = %d", rc)
	}
	var res []map[string]any
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad response line %q: %v", sc.Text(), err)
		}
		res = append(res, m)
	}
	return res
}

// fakeMCPMemd serves handler on a fresh unix socket and returns its path.
func fakeMCPMemd(t *testing.T, h http.Handler) string {
	t.Helper()
	dir := shortSockDir(t)
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func toolResult(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	content := res["content"].([]any)
	isErr, _ := res["isError"].(bool)
	return content[0].(map[string]any)["text"].(string), isErr
}

const callSearch = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"foo","limit":3}}}`

func TestMemMCPProtocol(t *testing.T) {
	resp := mcpRun(t, "/nonexistent/sock",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"nope"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"zzz","arguments":{}}}`,
	)
	if len(resp) != 5 {
		t.Fatalf("got %d responses (notification must not be answered): %v", len(resp), resp)
	}
	init := resp[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v", init["protocolVersion"])
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("capabilities lack tools: %v", init["capabilities"])
	}

	tools := resp[1]["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, x := range tools {
		tool := x.(map[string]any)
		names = append(names, tool["name"].(string))
		if tool["description"] == "" || tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v lacks description or object schema", tool["name"])
		}
	}
	sort.Strings(names)
	want := []string{"get_observations", "get_tool_uses", "read_session", "save_memory", "search", "session_start_context",
		"smart_outline", "smart_search", "smart_unfold", "timeline", "work_state_read", "work_state_write"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", names, want)
	}

	if code := resp[2]["error"].(map[string]any)["code"]; code != float64(-32601) {
		t.Errorf("unknown method code = %v", code)
	}
	if code := resp[3]["error"].(map[string]any)["code"]; code != float64(-32700) {
		t.Errorf("parse error code = %v", code)
	}
	if code := resp[4]["error"].(map[string]any)["code"]; code != float64(-32602) {
		t.Errorf("unknown tool code = %v", code)
	}
}

func TestMemMCPForward(t *testing.T) {
	var gotPath, gotBody string
	status := 200
	reply := "result text"
	sock := fakeMCPMemd(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.Method+" "+r.URL.Path, string(b)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))

	tests := []struct {
		name    string
		status  int
		reply   string
		isError bool
	}{
		{"ok", 200, "result text", false},
		{"tool error", 400, "Error: bad ids", true},
		{"unknown", 404, "Error: unknown tool", true},
		{"go error", 500, "boom", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reply = tt.status, tt.reply
			text, isErr := toolResult(t, mcpRun(t, sock, callSearch)[0])
			if text != tt.reply || isErr != tt.isError {
				t.Errorf("text=%q isError=%v, want %q %v", text, isErr, tt.reply, tt.isError)
			}
			if gotPath != "POST /tool/search" {
				t.Errorf("request = %q", gotPath)
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(gotBody), &args); err != nil || args["query"] != "foo" || args["limit"] != float64(3) {
				t.Errorf("body = %q (%v)", gotBody, err)
			}
		})
	}

	// The daemon runs in its own directory, so it learns the client's checkout from cwd:
	// CLAUDE_PROJECT_DIR, else the MCP process directory.
	t.Run("missing arguments sends the client checkout as cwd", func(t *testing.T) {
		status, reply = 200, "ok"
		cwd, _ := os.Getwd()
		t.Setenv("CLAUDE_PROJECT_DIR", "")
		mcpRun(t, sock, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"work_state_read"}}`)
		want, _ := json.Marshal(map[string]any{"cwd": cwd})
		if gotPath != "POST /tool/work_state_read" || gotBody != string(want) {
			t.Errorf("request = %q %q, want body %s", gotPath, gotBody, want)
		}
	})

	t.Run("CLAUDE_PROJECT_DIR wins over the process directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CLAUDE_PROJECT_DIR", dir)
		for _, call := range []string{
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"work_state_write","arguments":{"list":"l","fields":{"k":"v"}}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"save_memory","arguments":{"text":"t"}}}`,
		} {
			mcpRun(t, sock, call)
			var args map[string]any
			json.Unmarshal([]byte(gotBody), &args)
			if args["cwd"] != dir {
				t.Errorf("%s: cwd = %v, want %q", gotPath, args["cwd"], dir)
			}
		}
	})

	t.Run("session_start_context defaults project to the client checkout", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CLAUDE_PROJECT_DIR", dir)
		mcpRun(t, sock, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"session_start_context","arguments":{}}}`)
		var args map[string]any
		json.Unmarshal([]byte(gotBody), &args)
		if args["project"] != projectKey(dir) {
			t.Errorf("project = %v, want %q", args["project"], projectKey(dir))
		}
	})
}

func TestMemMCPMemdDown(t *testing.T) {
	hang := func() string {
		dir := shortSockDir(t)
		sock := filepath.Join(dir, "h.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { <-done; c.Close() }()
			}
		}()
		t.Cleanup(func() { close(done); ln.Close() })
		return sock
	}
	tests := []struct {
		name string
		sock func() string
	}{
		{"socket absent", func() string { return filepath.Join(os.TempDir(), "no-such-memory.sock") }},
		{"socket never answers", hang},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			resp := mcpRun(t, tt.sock(), callSearch)
			if d := time.Since(start); d > 2500*time.Millisecond {
				t.Errorf("took %v, budget 2.5s", d)
			}
			text, isErr := toolResult(t, resp[0])
			if !isErr || !strings.Contains(text, "memory") {
				t.Errorf("text=%q isError=%v, want isError naming memory", text, isErr)
			}
		})
	}
}

func TestMemMCPSmartLocal(t *testing.T) {
	wd, _ := os.Getwd()
	t.Setenv("CLAUDE_PROJECT_DIR", wd)
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"smart_outline","arguments":{"file_path":"testdata/memory/sample.go"}}}`
	text, isErr := toolResult(t, mcpRun(t, "/nonexistent/sock", call)[0])
	if isErr || !strings.Contains(text, "Shout (L19-21)") {
		t.Errorf("smart_outline via MCP: isError=%v text=%q", isErr, text)
	}
	bad := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"smart_unfold","arguments":{"file_path":"testdata/memory/absent.go","symbol_name":"x"}}}`
	text, isErr = toolResult(t, mcpRun(t, "/nonexistent/sock", bad)[0])
	if !isErr || text == "" {
		t.Errorf("smart_unfold on missing file: isError=%v text=%q", isErr, text)
	}
}
