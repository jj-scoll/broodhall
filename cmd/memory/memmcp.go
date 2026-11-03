package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// MCP stdio server for memory. JSON-RPC 2.0, one message per line. The eight
// DB-backed tools are forwarded to the daemon's unix socket (POST /tool/{name}); the three
// smart_* tools run locally in this process.

const (
	mcpDefaultVersion = "2025-06-18"
	mcpMemdBudget     = 2 * time.Second // whole round trip to memory, dial included
	mcpMaxLine        = 16 << 20
	mcpMaxReply       = 4 << 20
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// mcpSmart are the tools served in-process rather than by memory.
var mcpSmart = map[string]func(map[string]any) (string, error){
	"smart_search":  smartSearch,
	"smart_outline": smartOutline,
	"smart_unfold":  smartUnfold,
}

// memMCP serves MCP over in/out until in reaches EOF and returns the process exit code.
// sock is memory's unix socket path.
func memMCP(in io.Reader, out io.Writer, sock string) int {
	client := &http.Client{
		Timeout: mcpMemdBudget,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives: true,
		},
	}
	r := bufio.NewReaderSize(in, 64<<10)
	enc := json.NewEncoder(out)
	for {
		line, err := readMCPLine(r)
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := mcpHandle(client, line); resp != nil {
				if werr := enc.Encode(resp); werr != nil {
					return 1
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			fmt.Fprintln(os.Stderr, "mcp: read:", err)
			return 1
		}
	}
}

// readMCPLine reads one line, discarding any excess past mcpMaxLine (the oversize line is
// returned truncated and so fails to parse, which answers with a parse error).
func readMCPLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if len(line) < mcpMaxLine {
			line = append(line, chunk...)
		}
		if err != nil || !isPrefix {
			return line, err
		}
	}
}

func mcpHandle(client *http.Client, line []byte) *mcpResponse {
	var req mcpRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return &mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpError{-32700, "parse error"}}
	}
	if len(req.ID) == 0 { // notification: no reply
		return nil
	}
	resp := &mcpResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = mcpDefaultVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "memory", "version": "0.1.0"},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &mcpError{-32602, "invalid params"}
			break
		}
		if !mcpKnownTool(p.Name) {
			resp.Error = &mcpError{-32602, "unknown tool: " + p.Name}
			break
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		text, isErr := mcpCall(client, p.Name, p.Arguments)
		resp.Result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isErr,
		}
	default:
		resp.Error = &mcpError{-32601, "method not found: " + req.Method}
	}
	return resp
}

func mcpKnownTool(name string) bool {
	for _, t := range mcpTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// mcpCall runs one tool and returns its text; isError is true for every non-200 memory
// status and for any failure to reach memory.
func mcpCall(client *http.Client, name string, args map[string]any) (text string, isErr bool) {
	if fn, ok := mcpSmart[name]; ok {
		out, err := fn(args)
		if err != nil {
			return err.Error(), true
		}
		return out, false
	}
	// memory runs in its own directory: tell it which checkout this client is in.
	cwd, _ := os.Getwd()
	if dir := memdProjectDir(cwd); dir != "" {
		if args["cwd"] == nil {
			args["cwd"] = dir
		}
		if name == "session_start_context" && args["project"] == nil && args["projects"] == nil {
			args["project"] = projectKey(dir)
		}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return "Error: arguments are not JSON-encodable: " + err.Error(), true
	}
	req, err := http.NewRequest(http.MethodPost, "http://memory/tool/"+name, bytes.NewReader(body))
	if err != nil {
		return "Error: " + err.Error(), true
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "Error: memory is not reachable (" + mcpShortErr(err) + "). Start it with `memory serve`.", true
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, mcpMaxReply))
	if err != nil {
		return "Error: memory reply failed (" + mcpShortErr(err) + ")", true
	}
	return string(b), resp.StatusCode != http.StatusOK
}

func mcpShortErr(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out after " + mcpMemdBudget.String()
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return oe.Err.Error()
	}
	return strings.TrimSpace(err.Error())
}

// mcpTools: names, descriptions and input schemas.
var mcpTools = []mcpTool{
	{"search", "Smart search (default), step 1: search memory for an index with IDs. Review it, use timeline for context, then get_observations only for selected IDs. Params: query, limit, project, platformSource, type, obs_type, dateStart, dateEnd, offset, orderBy",
		json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query"},"limit":{"type":"number","description":"Max results (default 20)"},"project":{"type":"string","description":"Worker project name"},"projectId":{"type":"string","description":"Server project ID; defaults to the configured server project"},"platformSource":{"type":"string","description":"Filter by platform source (e.g. claude, codex, cursor) — restricts results to that agent's own memory"},"type":{"type":"string","description":"Document category to search: 'observations', 'sessions', or 'prompts' (default: all). Any other value is treated as an observation-type filter (alias for obs_type)."},"obs_type":{"type":"string","description":"Filter observations by their type (e.g. bugfix, feature). Comma-separated for multiple."},"dateStart":{"type":"string","description":"Start date filter (ISO)"},"dateEnd":{"type":"string","description":"End date filter (ISO)"},"offset":{"type":"number","description":"Pagination offset"},"orderBy":{"type":"string","description":"Sort order: date_desc or date_asc"}},"additionalProperties":true}`)},
	{"timeline", "Step 2: Get context around results. Params: anchor (observation ID) OR query (finds anchor automatically), depth_before, depth_after, project",
		json.RawMessage(`{"type":"object","properties":{"anchor":{"type":["number","string"],"description":"Observation ID to center the timeline around (string ID in server runtime)"},"query":{"type":"string","description":"Query to find anchor automatically"},"depth_before":{"type":"number","description":"Items before anchor (default 3)"},"depth_after":{"type":"number","description":"Items after anchor (default 3)"},"project":{"type":"string","description":"Worker project name"},"projectId":{"type":"string","description":"Server project ID; defaults to the configured server project"}},"additionalProperties":true}`)},
	{"get_observations", "Step 3: Fetch selected useful details for the IDs chosen after reviewing the index and timeline. Any shortening is marked. Params: ids (required), orderBy, limit, project (worker) or projectId (server)",
		json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","items":{"type":["number","string"]},"description":"Array of selected observation IDs to fetch (string IDs in server runtime)"},"projectId":{"type":"string","description":"Server project ID; defaults to the configured server project"}},"required":["ids"],"additionalProperties":true}`)},
	{"get_tool_uses", "Exceptional follow-up (raw tool I/O, rarely needed): fetch the ORIGINAL tool_input/tool_response for tool calls you already identified. Requires ids — run search/timeline/get_observations first and pass only the ids you actually need; these payloads are large and unsummarized. ids accept numeric tool_uses ids or tool_use_id strings. Params: ids (required), limit, project, contentSessionId.",
		json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","items":{"type":["number","string"]},"description":"Tool-use ids to fetch (required). Numeric tool_uses.id or opaque tool_use_id strings."},"limit":{"type":"number","description":"Max rows to return"},"project":{"type":"string","description":"Filter by project name"},"contentSessionId":{"type":"string","description":"Filter to one content session"}},"required":["ids"],"additionalProperties":true}`)},
	{"work_state_read", "Read this project's to-do lists and working state written with work_state_write: every open item, or one list, with done and dropped items when includeClosed is true. Params: list, includeClosed.",
		json.RawMessage(`{"type":"object","properties":{"list":{"type":"string","description":"Read only this list"},"includeClosed":{"type":"boolean","description":"Also show done and dropped items and closed lists"}},"additionalProperties":false}`)},
	{"work_state_write", `Your canonical to-do list and working state for this project, kept across sessions: whatever is still open is shown at the start of every session. Each call appends one entry to a list. To-do item: fields {"task": "<name>", "status": "todo" | "doing" | "done" | "dropped", ...details}. State on the list itself: any other fields (the latest value of each key wins; null clears a key; "status": "done" closes the list). List "facts" holds durable facts for this project, list "user-facts" facts for every project. Returns what is still open in the list. Params: list (required), fields (required).`,
		json.RawMessage(`{"type":"object","properties":{"list":{"type":"string","description":"The to-do list or tracked thing this entry belongs to, e.g. \"release\" or \"auth-refactor\""},"fields":{"type":"object","description":"Keys to set. Include \"task\" to update a to-do item. Values are strings, numbers, booleans, or null to clear a key.","additionalProperties":{"type":["string","number","boolean","null"]}}},"required":["list","fields"],"additionalProperties":false}`)},
	{"save_memory", "Save an explicit durable note, decision, correction or handoff into memory. Use this for your own note taking after searching memory for duplicates. Include project when the note belongs to a specific project. Server runtime uses observation_add to write to the selected server.",
		json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"Durable memory text."},"title":{"type":"string","description":"Concise searchable title."},"project":{"type":"string","description":"Project receiving the note; defaults to the active checkout."},"metadata":{"type":"object","additionalProperties":true,"description":"Optional provenance or source metadata."}},"required":["text"],"additionalProperties":false}`)},
	{"session_start_context", "Render the exact worker-mode SessionStart context for a project. Calls /api/context/inject and returns the same text hooks inject at startup. Params: project OR projects, platformSource, full, colors.",
		json.RawMessage(`{"type":"object","properties":{"project":{"type":"string","description":"Project name, e.g. my-app/night-parsnip"},"projects":{"oneOf":[{"type":"array","items":{"type":"string"}},{"type":"string"}],"description":"Project chain for context injection. Array or comma-separated string; last project is treated as primary."},"platformSource":{"type":"string","description":"Optional platform source filter, e.g. claude, codex, cursor"},"full":{"type":"boolean","description":"When true, request full context instead of configured limits"},"colors":{"type":"boolean","description":"When true, request human terminal-color formatting"}},"additionalProperties":false}`)},
	{"smart_search", "Search codebase for symbols, functions, classes using tree-sitter AST parsing. Returns folded structural views with token counts. Use path parameter to scope the search.",
		json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search term — matches against symbol names, file names, and file content"},"path":{"type":"string","description":"Root directory to search (default: current working directory)"},"max_results":{"type":"number","description":"Maximum results to return (default: 20)"},"file_pattern":{"type":"string","description":"Substring filter for file paths (e.g. \".ts\", \"src/services\")"}},"required":["query"]}`)},
	{"smart_unfold", "Expand a specific symbol (function, class, method) from a file. Returns the full source code of just that symbol. Use after smart_search or smart_outline to read specific code.",
		json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Path to the source file"},"symbol_name":{"type":"string","description":"Name of the symbol to unfold (function, class, method, etc.)"}},"required":["file_path","symbol_name"]}`)},
	{"smart_outline", "Get structural outline of a file — shows all symbols (functions, classes, methods, types) with signatures but bodies folded. Much cheaper than reading the full file.",
		json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Path to the source file"}},"required":["file_path"]}`)},
}
