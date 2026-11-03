package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Durable key->value lists: rows are list-level state (no "task"), never closed by status.
const (
	wsFactsList     = "facts"
	wsUserFactsList = "user-facts"
)

// workStateAppend records one work_state row (a map of fields) for a named list.
func workStateAppend(db *sql.DB, project, listName string, row map[string]any) error {
	if listName == "" {
		return errors.New("work_state: empty list name")
	}
	b, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("work_state: encode row: %w", err)
	}
	return withWrite(func() error {
		_, err := db.Exec(`INSERT INTO work_state (project, list_name, fields, created_at)
			VALUES (?, ?, ?, CAST(? AS TIMESTAMP))`, project, listName, string(b), memdTS(time.Now()))
		if err != nil {
			return fmt.Errorf("work_state: append: %w", err)
		}
		return nil
	})
}

// wsList is the running fold of one list.
type wsList struct {
	state   map[string]any // list-level fields
	stateAt time.Time      // last list-level row
	items   map[string]*wsItem
	order   []string // task names, first-appearance order
	closed  bool     // closed by a list-level status "done"
	last    time.Time
	seq     int // row index of the last row, to order lists with equal times
}

// wsItem is one to-do item; closed items are kept (their status says why) so a reader can show them.
type wsItem struct {
	fields map[string]any
	at     time.Time
	closed bool
}

func newWsList() *wsList {
	return &wsList{state: map[string]any{}, items: map[string]*wsItem{}}
}

// workStateFold replays the project's work_state rows in id order and returns the open state per
// list. user-facts rows from every project are included. Semantics:
//
//   - a row with a "task" key updates that item: the latest value of each field wins, null clears
//     a field, and status "done" or "dropped" closes (removes) the item; a later row for the same
//     task starts a fresh item
//   - a row without "task" updates list-level state the same way, and status "done" closes the
//     whole list (its state and all items are removed)
//   - facts and user-facts are never closed by status; status is an ordinary key there
//
// Each list maps to its list-level state map (when non-empty) followed by its open items in order
// of first appearance. Closed or empty lists are omitted.
func workStateFold(db *sql.DB, project string) (map[string][]map[string]any, error) {
	lists, err := workStateFoldRows(db, project)
	if err != nil {
		return nil, err
	}
	out := map[string][]map[string]any{}
	for name, l := range lists {
		if l.closed {
			continue
		}
		var res []map[string]any
		if len(l.state) > 0 {
			res = append(res, l.state)
		}
		for _, task := range l.order {
			if it := l.items[task]; !it.closed {
				row := map[string]any{"task": task}
				for k, v := range it.fields {
					row[k] = v
				}
				res = append(res, row)
			}
		}
		if len(res) > 0 {
			out[name] = res
		}
	}
	return out, nil
}

// workStateFoldRows is the shared replay behind workStateFold and the read tool. Closed items and
// closed lists stay in the result, flagged, with the time of their last row.
func workStateFoldRows(db *sql.DB, project string) (map[string]*wsList, error) {
	rows, err := db.Query(`SELECT list_name, fields, created_at FROM work_state
		WHERE project = ? OR list_name = ? ORDER BY id`, project, wsUserFactsList)
	if err != nil {
		return nil, fmt.Errorf("work_state: query: %w", err)
	}
	defer rows.Close()

	lists := map[string]*wsList{}
	for idx := 0; rows.Next(); idx++ {
		var name, raw string
		var at time.Time
		if err := rows.Scan(&name, &raw, &at); err != nil {
			return nil, fmt.Errorf("work_state: scan: %w", err)
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return nil, fmt.Errorf("work_state: decode row in %q: %w", name, err)
		}
		durable := name == wsFactsList || name == wsUserFactsList
		l := lists[name]
		if l == nil || (l.closed && !durable) {
			l = newWsList() // a row after the list closed starts it fresh
			lists[name] = l
		}
		l.last, l.seq = at, idx
		task, hasTask := wsTask(fields)
		switch {
		case hasTask && !durable:
			delete(fields, "task")
			s := wsStatus(fields)
			it := l.items[task]
			if it != nil && it.closed {
				l.dropItem(task) // a row after the item closed starts it fresh
				it = nil
			}
			if it == nil {
				it = &wsItem{fields: map[string]any{}}
				l.items[task] = it
				l.order = append(l.order, task)
			}
			wsMerge(it.fields, fields)
			it.at, it.closed = at, s == "done" || s == "dropped"
		case !durable && wsStatus(fields) == "done":
			wsMerge(l.state, fields)
			l.stateAt, l.closed = at, true
		default:
			wsMerge(l.state, fields)
			l.stateAt = at
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("work_state: rows: %w", err)
	}
	return lists, nil
}

func (l *wsList) dropItem(task string) {
	delete(l.items, task)
	for i, n := range l.order {
		if n == task {
			l.order = append(l.order[:i:i], l.order[i+1:]...)
			return
		}
	}
}

// wsTask returns the row's task name; a missing or null "task" means a list-level row.
func wsTask(fields map[string]any) (string, bool) {
	v, ok := fields["task"]
	if !ok || v == nil {
		return "", false
	}
	return fmt.Sprint(v), true
}

// wsStatus returns the row's status string ("" when absent or not a string).
func wsStatus(fields map[string]any) string {
	s, _ := fields["status"].(string)
	return s
}

// wsMerge applies fields onto dst: null deletes a key, anything else overwrites it.
func wsMerge(dst, fields map[string]any) {
	for k, v := range fields {
		if v == nil {
			delete(dst, k)
		} else {
			dst[k] = v
		}
	}
}

// ---- work_state_write / work_state_read tools ----
//
// Argument names and reply text are those of the work_state_write and work_state_read tools.

const (
	wsHandoffList     = "handoff"
	wsUserProject     = "__user__" // project key for user-facts rows
	wsMaxFieldsJSON   = 2000
	wsMaxHandoffJSON  = 4000
	wsMaxListName     = 200
	wsReplyBudget     = 1800
	wsMoreLinesSuffix = "work_state_read"
)

// wsCanonList lowercases the three reserved list names, which are matched
// case-insensitively; other names are kept as written.
func wsCanonList(name string) string {
	switch l := strings.ToLower(name); l {
	case wsFactsList, wsUserFactsList, wsHandoffList:
		return l
	}
	return name
}

// wsCwd is the checkout a tool call applies to: the cwd argument the MCP client adds. Never
// memory's own directory, which says nothing about the caller.
func wsCwd(args map[string]any) string { return qStr(args, "cwd") }

const wsNoCwd = "Error: cwd is required (the client's checkout); memory cannot tell which project this is for"

// wsFields validates and redacts the fields argument: non-empty keys, string, finite number, bool
// or null values, at least one key.
func wsFields(v any) (map[string]any, string) {
	in, ok := v.(map[string]any)
	if !ok || len(in) == 0 {
		return nil, "Error: fields must be an object that sets at least one key"
	}
	out := make(map[string]any, len(in))
	for k, x := range in {
		if k == "" {
			return nil, "Error: fields keys must not be empty"
		}
		switch t := x.(type) {
		case int:
			x = float64(t)
		case int64:
			x = float64(t)
		}
		switch t := x.(type) {
		case nil, bool:
			out[k] = t
		case string:
			out[k] = memdRedact(t)
		case float64:
			if math.IsNaN(t) || math.IsInf(t, 0) || (t == math.Trunc(t) && math.Abs(t) > 1<<53-1) {
				return nil, fmt.Sprintf("Error: fields[%q] must be a safe finite number", k)
			}
			out[k] = t
		default:
			return nil, fmt.Sprintf("Error: fields[%q] must be a string, number, boolean or null", k)
		}
	}
	return out, ""
}

// memWorkStateWrite appends one entry to a list and replies with what is still open in it.
func memWorkStateWrite(db *sql.DB, args map[string]any) (string, error) {
	list, _ := args["list"].(string)
	list = strings.TrimSpace(list)
	if list == "" || utf16Len(list) > wsMaxListName {
		return fmt.Sprintf("Error: list must be a non-empty string of at most %d characters", wsMaxListName), nil
	}
	list = wsCanonList(list)
	fields, bad := wsFields(args["fields"])
	if bad != "" {
		return bad, nil
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("work_state: encode fields: %w", err)
	}
	limit := wsMaxFieldsJSON
	if list == wsHandoffList {
		limit = wsMaxHandoffJSON
	}
	if n := utf16Len(string(b)); n > limit {
		return fmt.Sprintf("Error: fields must be at most %d characters as JSON", limit), nil
	}

	cwd := wsCwd(args)
	if cwd == "" {
		return wsNoCwd, nil
	}
	project := projectKey(cwd)
	userScope := list == wsUserFactsList
	row := project
	if userScope {
		row = wsUserProject
	}
	if err := workStateAppend(db, row, list, fields); err != nil {
		return "", err
	}
	if list == wsHandoffList {
		return fmt.Sprintf("Handoff stored for %s; the next session there sees it once, at startup.", project), nil
	}

	lists, err := workStateFoldRows(db, project)
	if err != nil {
		return "", err
	}
	scope := project
	if userScope {
		scope = "all projects"
	}
	head := fmt.Sprintf("Saved to %q in %s.", list, scope)
	var lines []string
	if l := lists[list]; l != nil {
		lines = wsRenderList(list, l, false, time.Now())
	}
	if len(lines) == 0 {
		return head + " Nothing in it is open now.", nil
	}
	return wsTruncate(head+" Still open in it:", lines, wsReplyBudget), nil
}

// memWorkStateRead lists the project's open to-dos and state, or one list.
func memWorkStateRead(db *sql.DB, args map[string]any) (string, error) {
	return memWorkStateReadAt(db, args, time.Now())
}

func memWorkStateReadAt(db *sql.DB, args map[string]any, now time.Time) (string, error) {
	list := qStr(args, "list")
	closed, _ := args["includeClosed"].(bool)
	cwd := wsCwd(args)
	if cwd == "" {
		return wsNoCwd, nil
	}
	project := projectKey(cwd)
	lists, err := workStateFoldRows(db, project)
	if err != nil {
		return "", err
	}
	var keep []string
	for name := range lists {
		switch {
		case list != "":
			if !strings.EqualFold(name, list) {
				continue
			}
		case name == wsHandoffList:
			continue // shown once at session start, not in the general read
		}
		keep = append(keep, name)
	}
	sort.Slice(keep, func(i, j int) bool {
		a, b := lists[keep[i]], lists[keep[j]]
		if !a.last.Equal(b.last) {
			return a.last.After(b.last)
		}
		return a.seq > b.seq
	})
	var out []string
	for _, name := range keep {
		out = append(out, wsRenderList(name, lists[name], closed, now)...)
	}
	if len(out) > 0 {
		return strings.Join(out, "\n"), nil
	}
	in := ""
	if list != "" {
		in = fmt.Sprintf(" in %q", list)
	}
	if closed {
		return fmt.Sprintf("No work state recorded%s for %s.", in, project), nil
	}
	return fmt.Sprintf("No open items%s for %s; set includeClosed to include finished ones.", in, project), nil
}

// wsClosedStatus reports whether a status value marks done or dropped work.
func wsClosedStatus(v any) bool {
	if v == nil {
		return false
	}
	s := strings.ToLower(wsStr(v))
	return s == "done" || s == "dropped"
}

// wsStr renders a field value as JavaScript's String() would.
func wsStr(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// wsKV renders "k=v, k=v" over the keys not in skip, sorted by key (rows are stored with sorted keys).
func wsKV(fields map[string]any, skip ...string) string {
	var keys []string
next:
	for k, v := range fields {
		if v == nil {
			continue
		}
		for _, s := range skip {
			if k == s {
				continue next
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + wsStr(fields[k])
	}
	return strings.Join(parts, ", ")
}

// wsAgo renders age: minutes under an hour, then "about N hours", then "about N days".
func wsAgo(d time.Duration) string {
	m := int(math.Max(1, math.Round(d.Minutes())))
	if m < 60 {
		return fmt.Sprintf("%d minute%s", m, plural(m))
	}
	h := int(math.Round(float64(m) / 60))
	if h < 48 {
		return fmt.Sprintf("about %d hour%s", h, plural(h))
	}
	return fmt.Sprintf("about %d days", int(math.Round(float64(h)/24)))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// wsRenderList renders a list: a "- list: state, updated ..." line then one line per item.
// An open-view read omits a closed list; items marked done or dropped show only with includeClosed.
func wsRenderList(name string, l *wsList, includeClosed bool, now time.Time) []string {
	if l.closed && !includeClosed {
		return nil
	}
	var items []string
	for _, task := range l.order {
		it := l.items[task]
		if !includeClosed && wsClosedStatus(it.fields["status"]) {
			continue
		}
		line := "  - "
		if s := it.fields["status"]; s != nil {
			line += "[" + wsStr(s) + "] "
		}
		line += task
		if extra := wsKV(it.fields, "task", "status"); extra != "" {
			line += " (" + extra + ")"
		}
		items = append(items, line+", updated "+wsAgo(now.Sub(it.at))+" ago")
	}
	// task and status are ordinary keys in the durable lists (R33): shown, and never hiding the line.
	durable := name == wsFactsList || name == wsUserFactsList
	var state string
	if durable {
		state = wsKV(l.state)
	} else {
		state = wsKV(l.state, "task")
	}
	showState := state != "" && (includeClosed || durable || !wsClosedStatus(l.state["status"]))
	if !showState && len(items) == 0 {
		return nil
	}
	head := "- " + name
	if showState {
		head += ": " + state + ", updated " + wsAgo(now.Sub(l.stateAt)) + " ago"
	}
	return append([]string{head}, items...)
}

// wsTruncate keeps the head plus as many lines as fit in max (UTF-16 units), else a
// "...N more lines" pointer.
func wsTruncate(head string, lines []string, max int) string {
	cur := head
	for i, ln := range lines {
		left := len(lines) - i
		more := fmt.Sprintf("\n- ...%d more line%s; read them with %s", left, plural(left), wsMoreLinesSuffix)
		next := cur + "\n" + ln
		extra := 0
		if i < len(lines)-1 {
			extra = utf16Len(more)
		}
		if utf16Len(next)+extra > max {
			return cur + more
		}
		cur = next
	}
	return cur
}
