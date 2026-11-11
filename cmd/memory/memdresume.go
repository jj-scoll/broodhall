package main

// `memory resume`: find a session in any Claude config directory and print the command that
// resumes it. The daemon holds the database, so the CLI asks it over the unix socket
// (GET /resume); the daemon only reads. Nothing here runs claude.

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// resumeItem is one resumable session.
type resumeItem struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Project    string    `json:"project"`
	Cwd        string    `json:"cwd"`
	Branch     string    `json:"branch"`
	ConfigDir  string    `json:"config_dir"`
	ForkedFrom string    `json:"forked_from"`
	LastSeen   time.Time `json:"last_seen"`
	Prompts    int       `json:"prompts"`
	Forks      int       `json:"forks"`
}

// resumeAmbiguous is returned when an id prefix names several sessions.
type resumeAmbiguous struct{ IDs []string }

func (e *resumeAmbiguous) Error() string {
	return "ambiguous: " + strings.Join(e.IDs, ", ")
}

var errResumeNotFound = errors.New("no session matches")

// resumeConfigDir is the Claude config directory a transcript path lives in:
// <config dir>/projects/<slug>/<session>.jsonl. It is "" for any other layout.
func resumeConfigDir(path string) string {
	if i := strings.LastIndex(path, "/projects/"); i > 0 {
		return path[:i]
	}
	return ""
}

// resumeBase selects the sessions that can be resumed: not observers, and with a transcript
// of their own. A session known only from subagent transcripts (a legacy row named after the
// subagent) has none, so it is never listed.
const resumeBase = ` WHERE NOT s.is_observer AND EXISTS (SELECT 1 FROM transcript_offsets o
	WHERE o.session_id = s.session_id AND NOT o.sub AND NOT o.observer)`

// resumeSearch finds resumable sessions matching every word and "quoted phrase" of q, newest
// first. A unit matches through the raw index (prompts, replies, tool inputs, file paths;
// whole words, see rawSearch), or as text (case-insensitive) in the summary request, project
// or directory. An empty q matches all; a q with no words (only punctuation) matches none.
func resumeSearch(db *sql.DB, q string, limit int) ([]resumeItem, error) {
	units := parseRawQuery(q)
	if len(units) == 0 && strings.TrimSpace(q) != "" {
		return []resumeItem{}, nil
	}
	ids := map[string]int{}
	if len(units) > 0 {
		if err := rawCatchUp(db); err != nil {
			return nil, fmt.Errorf("raw search index: %w", err)
		}
		var err error
		if ids, _, err = rawTermIDs(db, units); err != nil {
			return nil, err
		}
	}
	where, args := resumeBase, []any{}
	for _, u := range units {
		indexed := "false"
		if resumeUnitIndexed(u, ids) {
			indexed = "s.session_id IN (SELECT d.session_id FROM " + rawUnitFrom(u, ids, true) + ")"
		}
		pat := "%" + likeEscape(strings.Join(u, " ")) + "%"
		where += ` AND (` + indexed + `
			OR EXISTS (SELECT 1 FROM summaries m WHERE m.session_id = s.session_id AND m.request ILIKE ? ESCAPE '\')
			OR s.project ILIKE ? ESCAPE '\' OR s.cwd ILIKE ? ESCAPE '\')`
		args = append(args, pat, pat, pat)
	}
	return resumeQuery(db, where, args, limit)
}

// resumeUnitIndexed reports whether every term of u is in the raw index's dictionary.
func resumeUnitIndexed(u rawUnit, ids map[string]int) bool {
	for _, t := range u {
		if _, ok := ids[t]; !ok {
			return false
		}
	}
	return true
}

// resumeQuery runs the session select with a WHERE clause built on resumeBase.
func resumeQuery(db *sql.DB, where string, args []any, limit int) ([]resumeItem, error) {
	rows, err := db.Query(`SELECT s.session_id, s.project, s.cwd, coalesce(s.git_branch, ''), coalesce(s.forked_from, ''), s.last_seen_at,
	coalesce((SELECT substr(p.text, 1, 600) FROM prompts p WHERE p.session_id = s.session_id ORDER BY p.created_at, p.id LIMIT 1), ''),
	coalesce((SELECT substr(m.request, 1, 600) FROM summaries m WHERE m.session_id = s.session_id AND m.request <> '' ORDER BY m.id DESC LIMIT 1), ''),
	CAST((SELECT count(*) FROM prompts p WHERE p.session_id = s.session_id) AS INTEGER),
	CAST((SELECT count(*) FROM sessions f WHERE f.forked_from = s.session_id) AS INTEGER),
	(SELECT o.path FROM transcript_offsets o WHERE o.session_id = s.session_id AND NOT o.sub AND NOT o.observer ORDER BY o.updated_at DESC, o.path LIMIT 1)
	FROM sessions s`+where+` ORDER BY s.last_seen_at DESC, s.session_id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("resume query: %w", err)
	}
	defer rows.Close()
	items := []resumeItem{}
	for rows.Next() {
		var it resumeItem
		var prompt, summary string
		var path sql.NullString
		if err := rows.Scan(&it.ID, &it.Project, &it.Cwd, &it.Branch, &it.ForkedFrom, &it.LastSeen,
			&prompt, &summary, &it.Prompts, &it.Forks, &path); err != nil {
			return nil, err
		}
		it.Title = resumeTitle(prompt, summary)
		it.ConfigDir = resumeConfigDir(path.String)
		items = append(items, it)
	}
	return items, rows.Err()
}

// resumeTitle is the first line of the first prompt, else of the summary request.
func resumeTitle(prompt, summary string) string {
	for _, s := range []string{prompt, summary} {
		for _, line := range strings.Split(s, "\n") {
			if line = strings.TrimSpace(cleanLine(line)); line != "" {
				if utf8.RuneCountInString(line) > 100 {
					line = string([]rune(line)[:99]) + "…"
				}
				return line
			}
		}
	}
	return "(no prompt)"
}

// cleanLine replaces control characters, so stored text cannot move the cursor or recolour
// a terminal.
func cleanLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// resumeResolve finds the one session ref names: an exact id or a unique id prefix
// (case-insensitive), else the parent of the subagent whose agent id ref names. via is that
// agent id in the second case. Unknown refs give errResumeNotFound; a prefix that fits
// several sessions gives *resumeAmbiguous.
func resumeResolve(db *sql.DB, ref string) (it resumeItem, via string, err error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return it, "", errResumeNotFound
	}
	items, err := resumeQuery(db, resumeBase+` AND lower(s.session_id) LIKE ? ESCAPE '\'`, []any{likeEscape(ref) + "%"}, 50)
	if err != nil {
		return it, "", err
	}
	for _, c := range items {
		if strings.ToLower(c.ID) == ref {
			return c, "", nil
		}
	}
	switch {
	case len(items) == 1:
		return items[0], "", nil
	case len(items) > 1:
		return it, "", &resumeAmbiguous{IDs: resumeIDsOf(items)}
	}
	agent := strings.TrimPrefix(ref, "agent-")
	rows, err := db.Query(`SELECT path, session_id FROM transcript_offsets WHERE sub
		AND lower(path) LIKE ? ESCAPE '\' ORDER BY updated_at DESC, path LIMIT 50`, "%/subagents/agent-"+likeEscape(agent)+"%")
	if err != nil {
		return it, "", fmt.Errorf("resume subagent lookup: %w", err)
	}
	defer rows.Close()
	var parents []string
	for rows.Next() {
		var path, sid string
		if err := rows.Scan(&path, &sid); err != nil {
			return it, "", err
		}
		parent := transcriptSessionID(path)
		if strings.HasPrefix(parent, "agent-") { // no session directory above it
			parent = sid
		}
		if via == "" {
			via = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), filepath.Ext(path))
		}
		if !containsStr(parents, parent) {
			parents = append(parents, parent)
		}
	}
	if err := rows.Err(); err != nil {
		return it, "", err
	}
	if len(parents) > 1 {
		return it, "", &resumeAmbiguous{IDs: parents}
	}
	if len(parents) == 0 {
		return it, "", errResumeNotFound
	}
	rows.Close()
	ps, err := resumeQuery(db, resumeBase+` AND s.session_id = ?`, []any{parents[0]}, 1)
	if err != nil {
		return it, "", err
	}
	if len(ps) == 0 {
		return it, "", errResumeNotFound
	}
	return ps[0], via, nil
}

func resumeIDsOf(items []resumeItem) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// registerResume serves GET /resume?id=REF (one session) and GET /resume?q=WORDS&limit=N.
func registerResume(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("GET /resume", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("id") {
			it, via, err := resumeResolve(db, q.Get("id"))
			var amb *resumeAmbiguous
			switch {
			case errors.Is(err, errResumeNotFound):
				writeText(w, 404, "Error: no session matches "+q.Get("id"))
			case errors.As(err, &amb):
				writeText(w, 409, "Error: "+amb.Error())
			case err != nil:
				writeText(w, 500, err.Error())
			default:
				writeJSON(w, 200, map[string]any{"item": it, "via": via})
			}
			return
		}
		limit := 20
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 {
				writeText(w, 400, "Error: limit must be 1 to 200")
				return
			}
			limit = n
		}
		items, err := resumeSearch(db, q.Get("q"), limit)
		if err != nil {
			writeText(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
	})
}

// resumeCmdOpts are the facts about the shell the command will run in.
type resumeCmdOpts struct {
	Fork         bool
	Home         string            // the user's home directory
	EnvConfigDir string            // CLAUDE_CONFIG_DIR as this shell has it
	DirExists    func(string) bool // nil means os.Stat
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote quotes s for a POSIX shell: as it is when it holds only safe characters, else in
// single quotes with each embedded quote written as '\”.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resumeCommand is the shell command that resumes it, and a note when it had to guess. The
// config directory is left out only when both the session and this shell use ~/.claude;
// otherwise a shell pointing at another account would resume in the wrong one.
func resumeCommand(it resumeItem, o resumeCmdOpts) (cmd, note string) {
	exists := o.DirExists
	if exists == nil {
		exists = func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }
	}
	def := filepath.Join(o.Home, ".claude")
	cur := o.EnvConfigDir
	if cur == "" {
		cur = def
	}
	var b strings.Builder
	switch {
	case it.Cwd == "":
		note = "the session has no working directory recorded; run this from its project directory"
	case !exists(it.Cwd):
		note = it.Cwd + " no longer exists; resuming from the current directory, and claude may not find the session there (it looks sessions up by project directory)"
	default:
		b.WriteString("cd -- " + shellQuote(it.Cwd) + " && ")
	}
	if cfg := it.ConfigDir; cfg != "" && !(filepath.Clean(cfg) == filepath.Clean(def) && filepath.Clean(cur) == filepath.Clean(def)) {
		b.WriteString("CLAUDE_CONFIG_DIR=" + shellQuote(cfg) + " ")
	}
	b.WriteString("claude --resume " + shellQuote(it.ID))
	if o.Fork {
		b.WriteString(" --fork-session")
	}
	return b.String(), note
}

// resumeAgo is t as a short age relative to now.
func resumeAgo(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 45*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dw ago", int(d/(7*24*time.Hour)))
	}
	return fmt.Sprintf("%dmo ago", int(d/(30*24*time.Hour)))
}

// renderResumeList writes the numbered list and returns the sessions in the order numbered.
// A fork whose original is in the list is indented under it; a group sorts by its newest
// member, so a fresh fork lifts its original to the top.
func renderResumeList(w io.Writer, items []resumeItem, now time.Time, home string) []resumeItem {
	byID := map[string]int{}
	for i, it := range items {
		byID[it.ID] = i
	}
	root := func(i int) int { // the topmost original present in the list
		for hops := 0; hops < len(items); hops++ {
			j, ok := byID[items[i].ForkedFrom]
			if !ok || items[i].ForkedFrom == "" || j == i {
				break
			}
			i = j
		}
		return i
	}
	type group struct {
		head  int
		kids  []int
		newer time.Time
	}
	groups := map[int]*group{}
	for i, it := range items {
		r := root(i)
		g := groups[r]
		if g == nil {
			g = &group{head: r}
			groups[r] = g
		}
		if i != r {
			g.kids = append(g.kids, i)
		}
		if it.LastSeen.After(g.newer) {
			g.newer = it.LastSeen
		}
	}
	var gs []*group
	for _, g := range groups {
		sort.Slice(g.kids, func(a, b int) bool { return items[g.kids[a]].LastSeen.After(items[g.kids[b]].LastSeen) })
		gs = append(gs, g)
	}
	sort.Slice(gs, func(a, b int) bool {
		if !gs[a].newer.Equal(gs[b].newer) {
			return gs[a].newer.After(gs[b].newer)
		}
		return items[gs[a].head].ID < items[gs[b].head].ID
	})
	var order []resumeItem
	row := func(i int, indent string) {
		it := items[i]
		order = append(order, it)
		fmt.Fprintf(w, "%s%d) %s", indent, len(order), it.Title)
		switch it.Forks {
		case 0:
		case 1:
			fmt.Fprint(w, "  (1 fork)")
		default:
			fmt.Fprintf(w, "  (%d forks)", it.Forks)
		}
		fmt.Fprintln(w)
		parts := []string{it.ID}
		if it.Project != "" {
			parts = append(parts, cleanLine(it.Project))
		}
		if it.Branch != "" {
			parts = append(parts, cleanLine(it.Branch))
		}
		parts = append(parts, resumeAgo(now, it.LastSeen), fmt.Sprintf("%d prompts", it.Prompts))
		if it.ConfigDir != "" {
			parts = append(parts, cleanLine(tildePath(it.ConfigDir, home)))
		}
		fmt.Fprintf(w, "%s   %s\n", indent, strings.Join(parts, "  "))
	}
	for _, g := range gs {
		row(g.head, "")
		for _, k := range g.kids {
			row(k, "    ")
		}
	}
	return order
}

func tildePath(p, home string) string {
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// memResumeTo is `memory resume [--list] [--fork] [--limit N] [query or session id]`. A single
// word is tried as a session id (or id prefix, or subagent id) first, and printed as a
// command. Otherwise the matches are listed; on a terminal (tty) the list goes to errw and
// a number read from in picks one, so `eval "$(memory resume foo)"` works. Notes go to errw
// and only the command to out.
func memResumeTo(args []string, sock string, in io.Reader, tty bool, out, errw io.Writer) int {
	fs := flag.NewFlagSet("memory resume", flag.ContinueOnError)
	fs.SetOutput(errw)
	list := fs.Bool("list", false, "list matching sessions instead of printing a command")
	fork := fs.Bool("fork", false, "resume as a new forked session (adds --fork-session)")
	limit := fs.Int("limit", 20, "most sessions to list")
	var words []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		words, rest = append(words, fs.Arg(0)), fs.Args()[1:]
	}
	if *limit < 1 || *limit > 200 {
		fmt.Fprintln(errw, "memory resume: --limit must be 1 to 200")
		return 2
	}
	home, _ := os.UserHomeDir()
	opts := resumeCmdOpts{Fork: *fork, Home: home, EnvConfigDir: os.Getenv("CLAUDE_CONFIG_DIR")}
	fail := func(err error) int {
		fmt.Fprintf(errw, "memory resume: %v (is `memory serve` running?)\n", err)
		return 1
	}
	printCmd := func(it resumeItem, via string) int {
		cmd, note := resumeCommand(it, opts)
		if via != "" {
			fmt.Fprintf(errw, "memory resume: subagent %s belongs to session %s; resuming that session\n", via, it.ID)
		}
		if note != "" {
			fmt.Fprintf(errw, "memory resume: %s\n", note)
		}
		fmt.Fprintln(out, cmd)
		return 0
	}

	if !*list && len(words) == 1 {
		status, body, err := memGetStatus(sock, "/resume?id="+url.QueryEscape(words[0]), 10*time.Second)
		if err != nil {
			return fail(err)
		}
		switch status {
		case http.StatusOK:
			var r struct {
				Item resumeItem `json:"item"`
				Via  string     `json:"via"`
			}
			if err := json.Unmarshal(body, &r); err != nil {
				fmt.Fprintf(errw, "memory resume: bad reply: %v\n", err)
				return 1
			}
			return printCmd(r.Item, r.Via)
		case http.StatusNotFound: // not an id: search for it
		default:
			fmt.Fprintf(errw, "memory resume: %s\n", strings.TrimSpace(strings.TrimPrefix(string(body), "Error: ")))
			return 1
		}
	}

	query := strings.Join(words, " ")
	status, body, err := memGetStatus(sock, "/resume?limit="+strconv.Itoa(*limit)+"&q="+url.QueryEscape(query), 10*time.Second)
	if err != nil {
		return fail(err)
	}
	if status != http.StatusOK {
		fmt.Fprintf(errw, "memory resume: %s\n", strings.TrimSpace(string(body)))
		return 1
	}
	var res struct {
		Items []resumeItem `json:"items"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		fmt.Fprintf(errw, "memory resume: bad reply: %v\n", err)
		return 1
	}
	if len(res.Items) == 0 {
		if query == "" {
			fmt.Fprintln(errw, "memory resume: no sessions recorded yet")
		} else {
			fmt.Fprintf(errw, "memory resume: no sessions match %q\n", query)
		}
		return 1
	}
	now := time.Now()
	if *list || !tty {
		renderResumeList(out, res.Items, now, home)
		if !*list {
			fmt.Fprintln(errw, "memory resume: pass a session id to print its command")
		}
		return 0
	}
	if len(res.Items) == 1 {
		return printCmd(res.Items[0], "")
	}
	order := renderResumeList(errw, res.Items, now, home)
	fmt.Fprintf(errw, "Resume which? [1-%d] ", len(order))
	line, _ := bufio.NewReader(in).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(order) {
		fmt.Fprintln(errw, "\nmemory resume: no such entry")
		return 1
	}
	return printCmd(order[n-1], "")
}

// memResumeCmd is `memory resume` on the real streams and socket.
func memResumeCmd(args []string) int {
	fi, err := os.Stdin.Stat()
	tty := err == nil && fi.Mode()&os.ModeCharDevice != 0
	return memResumeTo(args, memdSockPath(), os.Stdin, tty, os.Stdout, os.Stderr)
}
