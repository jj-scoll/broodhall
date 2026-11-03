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
	{"search", "Find memories that match a text query. Returns a compact index of hits (observations, session summaries, prompts) with IDs, titles and rough read costs, not full records. Scan it, then use timeline or get_observations on the IDs worth reading. Optional filters: project, platformSource, type, obs_type, dateStart, dateEnd, offset, orderBy.",
		json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Text to search for"},"limit":{"type":"number","description":"Maximum number of hits (default 20)"},"project":{"type":"string","description":"Project name to restrict to"},"projectId":{"type":"string","description":"Project ID on a server; the configured project when omitted"},"platformSource":{"type":"string","description":"Only memories from this agent platform, e.g. claude or codex"},"type":{"type":"string","description":"Kind of record to search: 'observations', 'sessions' or 'prompts'; omit to search all. Any other value acts as an obs_type filter."},"obs_type":{"type":"string","description":"Only observations of these types (e.g. bugfix, feature); separate several with commas"},"dateStart":{"type":"string","description":"Earliest date, ISO format"},"dateEnd":{"type":"string","description":"Latest date, ISO format"},"offset":{"type":"number","description":"Number of hits to skip"},"orderBy":{"type":"string","description":"date_desc (newest first) or date_asc"}},"additionalProperties":true}`)},
	{"timeline", "Show what happened around one memory: the records just before and after it. Pass an anchor (an observation ID, or S<n> for a session) or a query whose best hit becomes the anchor, but not both. depth_before and depth_after size the window; project narrows it.",
		json.RawMessage(`{"type":"object","properties":{"anchor":{"type":["number","string"],"description":"Observation ID to center on, or S<n> for a session"},"query":{"type":"string","description":"Search text; its best hit becomes the anchor"},"depth_before":{"type":"number","description":"Records to show before the anchor (default 3)"},"depth_after":{"type":"number","description":"Records to show after the anchor (default 3)"},"project":{"type":"string","description":"Project name to restrict to"},"projectId":{"type":"string","description":"Project ID on a server; the configured project when omitted"}},"additionalProperties":true}`)},
	{"get_observations", "Read the full records for specific observation IDs, usually picked from search or timeline output. Long fields are cut and the cut is marked. Takes ids (required) and optionally orderBy, limit, project or projectId.",
		json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","items":{"type":["number","string"]},"description":"Observation IDs to read"},"projectId":{"type":"string","description":"Project ID on a server; the configured project when omitted"}},"required":["ids"],"additionalProperties":true}`)},
	{"get_tool_uses", "Last resort for exact tool input and output: returns the stored raw payloads of tool calls you have already located. They are large and unsummarized, so look up IDs first (search, timeline, get_observations) and request only what you need. ids may be numeric tool_uses IDs or tool_use_id strings. Optional: limit, project, contentSessionId.",
		json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","items":{"type":["number","string"]},"description":"Tool calls to return: numeric tool_uses IDs or tool_use_id strings"},"limit":{"type":"number","description":"Maximum number of rows"},"project":{"type":"string","description":"Only this project"},"contentSessionId":{"type":"string","description":"Only this content session"}},"required":["ids"],"additionalProperties":true}`)},
	{"work_state_read", "Show what work_state_write has stored: open to-dos across every list, or the contents of one list. Set includeClosed to also see done and dropped items and finished lists.",
		json.RawMessage(`{"type":"object","properties":{"list":{"type":"string","description":"Show only this list"},"includeClosed":{"type":"boolean","description":"Include done and dropped items and finished lists"}},"additionalProperties":false}`)},
	{"work_state_write", `Record to-dos and progress notes for this project. Entries persist across sessions, and anything still open is shown when a session starts. Each call appends one entry to a named list. For a to-do item, pass fields {"task": "<name>", "status": "todo" | "doing" | "done" | "dropped"} plus any detail keys. Other keys set state on the list itself: the newest value of a key wins, null removes it, and "status": "done" closes the list. The list "facts" keeps lasting facts about this project; "user-facts" keeps facts that apply to every project. The reply shows what remains open in that list. Required: list, fields.`,
		json.RawMessage(`{"type":"object","properties":{"list":{"type":"string","description":"The to-do list or tracked thing this entry belongs to, e.g. \"release\" or \"auth-refactor\""},"fields":{"type":"object","description":"Keys to set. Include \"task\" to update a to-do item. Values are strings, numbers, booleans, or null to clear a key.","additionalProperties":{"type":["string","number","boolean","null"]}}},"required":["list","fields"],"additionalProperties":false}`)},
	{"save_memory", "Store a note worth keeping: a decision, a correction, a handoff or any lasting fact. Search first so you do not duplicate an existing note. Pass project if the note belongs somewhere other than the current checkout.",
		json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"The note to keep"},"title":{"type":"string","description":"Short title to find it by"},"project":{"type":"string","description":"Project the note belongs to (default: the current checkout)"},"metadata":{"type":"object","additionalProperties":true,"description":"Optional details about where the note came from"}},"required":["text"],"additionalProperties":false}`)},
	{"session_start_context", "Return the SessionStart context text for a project, identical to what the startup hook injects. Pass project or projects; platformSource, full and colors are optional.",
		json.RawMessage(`{"type":"object","properties":{"project":{"type":"string","description":"Project name, e.g. my-app/night-parsnip"},"projects":{"oneOf":[{"type":"array","items":{"type":"string"}},{"type":"string"}],"description":"Several projects, as an array or comma-separated string; the last one is the primary project"},"platformSource":{"type":"string","description":"Optional platform filter, e.g. claude or codex"},"full":{"type":"boolean","description":"Return the full context instead of applying the configured limits"},"colors":{"type":"boolean","description":"Format the output with terminal colors"}},"additionalProperties":false}`)},
	{"smart_search", "Search source files for a symbol, file name or text, using tree-sitter parsing. Each matching file comes back as a folded outline with its token count. Use path to limit where it looks.",
		json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Term to match against symbol names, file names and file contents"},"path":{"type":"string","description":"Directory to search (default: the current directory)"},"max_results":{"type":"number","description":"Cap on the number of results (default 20)"},"file_pattern":{"type":"string","description":"Keep only file paths containing this text, e.g. \".go\" or \"cmd/\""}},"required":["query"]}`)},
	{"smart_unfold", "Return the complete source of one named symbol (a function, class or method) from a file. smart_search and smart_outline show which names exist.",
		json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Source file to read"},"symbol_name":{"type":"string","description":"Function, class or method to expand"}},"required":["file_path","symbol_name"]}`)},
	{"smart_outline", "List the symbols in a file (functions, classes, methods, types) with their signatures and the bodies folded away. Uses far fewer tokens than reading the whole file.",
		json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string","description":"Source file to read"}},"required":["file_path"]}`)},
}
