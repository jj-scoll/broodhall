package main

// SessionStart context block: a work-state section (header, Facts (all projects), Project
// facts, Still open), a header, legend and stats, a day-grouped timeline, the last summary and
// the hook output with its 9,500-byte guard. The number of observations shown is fixed at 20;
// SESSION_COUNT is 10.
//
// Deliberate differences, all forced by the byte budget or by missing data:
//   - titles, summary requests and last-summary fields are clipped (ctxTitleRunes, ctxFieldRunes)
//   - facts are listed in key order (Go maps carry no insertion order)
//   - the Stats line has no "work"/"savings" figures (memory stores no discovery tokens) and the
//     closing "Access Nk tokens" line is omitted for the same reason
//   - summaries sit in the timeline at their own created_at (an earlier design shifted it to the next
//     summary's time)
//   - the hook's systemMessage is out of scope for this block

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Budgets in JSON bytes of contextJSON's output. The host cuts anything over 10,000 to a 2 KB
// preview, so the guard is never exceeded; the target is where trimming stops.
const (
	contextTarget = 8800
	contextGuard  = 9500

	ctxObsCount     = 20
	ctxSessionCount = 10
	ctxTitleRunes   = 200
	ctxFieldRunes   = 500
	ctxCutMarker    = "\n[memory] Output was cut to stay under the SessionStart size cap; query memory for what is missing."

	ctxOpenBudget  = 1800
	ctxFactsBudget = 2000 // kUr
	ctxUserBudget  = 1500 // Lce
	ctxFactRunes   = 240  // CJe
	ctxLineUnits   = 400  // cap on one open-list line; item names use ctxTitleRunes
	ctxStateRunes  = 60   // SUr
	ctxStaleDays   = 14   // bUr
	ctxHideDays    = 90   // SJe
)

const ctxWorkHeader = `# Work state: to-do lists and notes that persist across sessions
Track to-dos and multi-step work with the work_state_write tool, in preference to any built-in task tool.
- A to-do item is an entry like {"task": "<name>", "status": "todo"|"doing"|"done"|"dropped"}; any other key sets state on the list (newest value wins, null removes the key, "status": "done" closes the list)
- Open lists are summarized below; work_state_read gives the detail (includeClosed=true adds finished items)
- Lasting facts go in the list "facts" (this project) or "user-facts" (every project), one key per fact, null to delete; both appear at the start of every session
- Produced something the user can open (PR, deck, page, report)? Put its URL or absolute path in a "work_product" field on that item; the memory UI shows it with the item`

// contextJSON is the SessionStart hook output. HTML escaping is off, as in JavaScript's
// JSON.stringify, so the byte count matches what the hook consumer measures.
func contextJSON(block string) []byte {
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = "SessionStart"
	out.HookSpecificOutput.AdditionalContext = block
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return []byte(`{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":""}}`)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

type ctxSummary struct {
	id                                            int64
	request                                       string
	investigated, learned, completed, next, notes string
	created                                       time.Time
}

type ctxEntry struct {
	at  time.Time
	obs *qObs
	sum *ctxSummary
}

// workData is the work-state section's raw material, rendered under a budget scale.
type workData struct {
	userFacts, projFacts, open []string
	err                        error // work_state could not be read
}

// buildContext renders the SessionStart block for project: work state, then the recent-context
// timeline (20 observations and 10 session summaries, oldest first) and the last summary. Whole
// timeline entries are dropped oldest first until the JSON fits contextTarget. If the work-state
// section alone is over contextGuard, its budgets are halved, then zeroed, and as a last resort
// the text is cut, so the JSON never exceeds contextGuard. The first open work_state item is
// always kept (clipped to ctxLineUnits), also on the shrink path. A work_state read error is
// stated in the block; observation and summary read errors are written to stderr and leave
// that part of the timeline out.
func buildContext(db *sql.DB, project string, now time.Time) string {
	return buildContextWith(db, project, now, nil)
}

// buildContextWith is buildContext with a claimed handoff block first. The handoff keeps its
// place and is the last thing cut: timeline entries go first, then the work-state budgets, and
// only then is the handoff text itself shortened (the guard trims the body and
// leaves the handoff, and counts bytes, not UTF-16 units).
func buildContextWith(db *sql.DB, project string, now time.Time, h *handoffEntry) string {
	return buildContextExcluding(db, project, now, h, "")
}

// buildContextExcluding also leaves out the compaction summaries of excludeSession (when not
// empty): after a compaction Claude Code already holds that session's summary in the
// conversation, so showing it again only spends tokens.
func buildContextExcluding(db *sql.DB, project string, now time.Time, h *handoffEntry, excludeSession string) string {
	wd := loadWorkData(db, project, now)
	obs, err := qObsQuery(db, `SELECT `+obsCols+` FROM observations WHERE project = ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, project, ctxObsCount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory context: observations: %v\n", err)
		obs = nil
	}
	if err := loadFiles(db, obs); err != nil {
		fmt.Fprintf(os.Stderr, "memory context: files: %v\n", err)
	} else {
		newStaleChecker().markStale(obs)
	}
	sums, err := loadCtxSummaries(db, project, excludeSession)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory context: summaries: %v\n", err)
		sums = nil
	}

	entries := make([]ctxEntry, 0, len(obs)+len(sums))
	for _, o := range obs {
		entries = append(entries, ctxEntry{at: o.created, obs: o})
	}
	for i := range sums {
		entries = append(entries, ctxEntry{at: sums[i].created, sum: &sums[i]})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].at.Equal(entries[j].at) {
			return entries[i].at.Before(entries[j].at)
		}
		return entries[i].obs != nil && entries[j].obs == nil
	})

	var last *ctxSummary
	if len(sums) > 0 && ctxShowLast(&sums[0], obs) {
		last = &sums[0]
	}
	assemble := func(es []ctxEntry, scale float64, handoffCap int) string {
		var parts []string
		if h != nil {
			parts = append(parts, h.render(now, handoffCap))
		}
		parts = append(parts, wd.render(scale))
		if len(es) > 0 {
			parts = append(parts, ctxBody(project, now, es, last))
		}
		return strings.Join(parts, "\n\n")
	}

	handoffCap := handoffMaxUnits
	for k := 0; k <= len(entries); k++ {
		if b := assemble(entries[k:], 1, handoffCap); k == len(entries) || ctxSize(b) <= contextTarget {
			if ctxSize(b) <= contextGuard {
				return b
			}
			break
		}
	}
	// Over the guard even with no timeline: shorten the handoff first (it is the one block whose
	// size is not bounded by a budget), then halve the work-state budgets.
	if h != nil {
		for _, limit := range []int{contextTarget, contextGuard} {
			for c := handoffCap; c >= handoffMinCap; c /= 2 {
				if b := assemble(nil, 1, c); ctxSize(b) <= limit {
					return b
				}
			}
		}
		handoffCap = handoffMinCap
	}
	for _, scale := range []float64{0.5, 0.25, 0} {
		if b := assemble(nil, scale, handoffCap); ctxSize(b) <= contextGuard {
			return b
		}
	}
	return ctxCut(assemble(nil, 0, handoffMinCap))
}

func ctxSize(block string) int { return len(contextJSON(block)) }

// ctxCut shortens block, on a rune boundary, until block+marker fits contextGuard.
func ctxCut(block string) string {
	lo, hi := 0, len(block)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if ctxSize(clipBytes(block, mid)+ctxCutMarker) <= contextGuard {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return clipBytes(block, lo) + ctxCutMarker
}

func loadCtxSummaries(db *sql.DB, project, excludeSession string) ([]ctxSummary, error) {
	rows, err := db.Query(`SELECT id, request, investigated, learned, completed, next_steps, notes, created_at
		FROM summaries WHERE project = ?
		AND (? = '' OR id NOT IN (SELECT summary_id FROM compactions WHERE session_id = ?))
		ORDER BY created_at DESC, id DESC LIMIT ?`, project, excludeSession, excludeSession, ctxSessionCount)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ctxSummary
	for rows.Next() {
		var s ctxSummary
		if err := rows.Scan(&s.id, &s.request, &s.investigated, &s.learned, &s.completed, &s.next, &s.notes, &s.created); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ctxShowLast: the newest summary shows when it has content and is not
// older than the newest observation.
func ctxShowLast(s *ctxSummary, obs []*qObs) bool {
	if s.investigated == "" && s.learned == "" && s.completed == "" && s.next == "" && s.notes == "" {
		return false
	}
	return len(obs) == 0 || !s.created.Before(obs[0].created)
}

func ctxBody(project string, now time.Time, es []ctxEntry, last *ctxSummary) string {
	nObs, tokens := 0, 0
	for _, e := range es {
		if e.obs != nil {
			nObs++
			tokens += readTokens(e.obs.title, e.obs.subtitle, e.obs.narrative, e.obs.facts)
		}
	}
	lines := []string{
		fmt.Sprintf("# [%s] recent context, %s", project, now.In(memdQueryTZ).Format("2006-01-02 3:04pm MST")),
		"",
		"Type markers: 🎯session ●bugfix ◆feature ↻refactor ✓change ○discovery ⚖decision ⚠security_alert ⚷security_note ⊘sensitive",
		"Each row: ID, time, type marker, title",
		"Read a record in full with get_observations([IDs]); find older ones with the search tool",
		"",
	}
	if nObs > 0 {
		lines = append(lines, fmt.Sprintf("Stats: %d obs (%st read)", nObs, ctxThousands(tokens)), "")
	}
	day, prevClock := "", ""
	for _, e := range es {
		if d := fmtDay(e.at); d != day {
			day, prevClock = d, ""
			lines = append(lines, "### "+d)
		}
		if e.sum != nil {
			req := ctxClip(e.sum.request, ctxTitleRunes)
			if req == "" {
				req = "Session started"
			}
			lines = append(lines, fmt.Sprintf("S%d %s (%s)", e.sum.id, req, fmtDayClock(e.at)))
			continue
		}
		clock := ctxShortClock(e.at)
		shown := `"`
		if clock != prevClock {
			shown = clock
		}
		prevClock = clock
		title := ctxClip(e.obs.title, ctxTitleRunes)
		if title == "" {
			title = "Untitled"
		}
		lines = append(lines, fmt.Sprintf("%d %s %s %s", e.obs.id, shown, memdTypeIcon(e.obs.typ), withStale(title, e.obs)))
	}
	if last != nil {
		for _, f := range [][2]string{{"Investigated", last.investigated}, {"Learned", last.learned},
			{"Completed", last.completed}, {"Next Steps", last.next}, {"Notes", last.notes}} {
			if v := ctxClip(f[1], ctxFieldRunes); v != "" {
				lines = append(lines, "**"+f[0]+"**: "+v, "")
			}
		}
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

var ctxClockShort = strings.NewReplacer(" am", "a", " pm", "p")

func ctxShortClock(t time.Time) string { return ctxClockShort.Replace(strings.ToLower(fmtClock(t))) }

func ctxThousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// ctxFlat puts a value on one line so it cannot break the line-per-entry layout.
func ctxFlat(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(strings.ToValidUTF8(s, "�"))
}

// ctxClip cuts s to n UTF-16 units, ending in "…".
func ctxClip(s string, n int) string {
	s = ctxFlat(s)
	if utf16Len(s) <= n {
		return s
	}
	var b strings.Builder
	units := 0
	for _, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > n-1 {
			break
		}
		b.WriteRune(r)
		units += w
	}
	return b.String() + "…"
}

func loadWorkData(db *sql.DB, project string, now time.Time) *workData {
	wd := &workData{}
	lists, err := workStateFoldRows(db, project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory context: work_state: %v\n", err)
		wd.err = err
		return wd
	}
	project_ := ctxFacts(lists[wsFactsList])
	wd.projFacts = ctxFactLines(project_)
	user := ctxFacts(lists[wsUserFactsList])
	for k := range project_ {
		delete(user, k) // hide user-facts a project fact overrides
	}
	wd.userFacts = ctxFactLines(user)

	var names []string
	for name := range lists {
		switch name {
		case wsFactsList, wsUserFactsList, wsHandoffList, "my-rules":
			continue
		}
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := lists[names[i]], lists[names[j]]
		if !a.last.Equal(b.last) {
			return a.last.After(b.last)
		}
		return a.seq > b.seq
	})
	hidden := 0
	for _, name := range names {
		l := lists[name]
		lines := ctxListLines(name, l, now)
		if len(lines) == 0 {
			continue
		}
		age := now.Sub(l.last)
		switch {
		case age > ctxHideDays*24*time.Hour:
			hidden++
		case age > ctxStaleDays*24*time.Hour:
			open := 0
			for _, it := range l.items {
				if !wsClosedStatus(it.fields["status"]) {
					open++
				}
			}
			n := ""
			if open > 0 {
				n = fmt.Sprintf("%d open, ", open)
			}
			wd.open = append(wd.open, fmt.Sprintf("- %s: %sstale %dd", name, n, int(age/(24*time.Hour))))
		default:
			wd.open = append(wd.open, lines...)
		}
	}
	if hidden > 0 {
		wd.open = append(wd.open, fmt.Sprintf("- ...%d list%s idle for %d+ days and hidden; work_state_read lists them", hidden, plural(hidden), ctxHideDays))
	}
	return wd
}

// ctxFacts is the list-level state of a durable list without "task".
func ctxFacts(l *wsList) map[string]any {
	out := map[string]any{}
	if l == nil {
		return out
	}
	for k, v := range l.state {
		if k != "task" && v != nil {
			out[k] = v
		}
	}
	return out
}

func ctxFactLines(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = "- " + ctxFlat(k) + ": " + ctxClip(wsStr(m[k]), ctxFactRunes)
	}
	return out
}

// ctxListLines renders a list in compact mode: the list line (state values cut to 60) and
// one line per open item, without item details.
func ctxListLines(name string, l *wsList, now time.Time) []string {
	if l.closed {
		return nil
	}
	var items []string
	for _, task := range l.order {
		it := l.items[task]
		if wsClosedStatus(it.fields["status"]) {
			continue
		}
		line := "  - "
		if s := it.fields["status"]; s != nil {
			line += "[" + ctxClip(wsStr(s), ctxStateRunes) + "] "
		}
		items = append(items, ctxClip(line+ctxClip(task, ctxTitleRunes)+", updated "+wsAgo(now.Sub(it.at))+" ago", ctxLineUnits))
	}
	var keys []string
	for k, v := range l.state {
		if v != nil && k != "task" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = ctxFlat(k) + "=" + ctxClip(wsStr(l.state[k]), ctxStateRunes)
	}
	state := strings.Join(parts, ", ")
	showState := state != "" && !wsClosedStatus(l.state["status"])
	if !showState && len(items) == 0 {
		return nil
	}
	head := "- " + ctxClip(name, ctxTitleRunes)
	if showState {
		head += ": " + state + ", updated " + wsAgo(now.Sub(l.stateAt)) + " ago"
	}
	return append([]string{ctxClip(head, ctxLineUnits)}, items...)
}

// render joins header, user facts, project facts and the open list, each under its
// budget times scale. Scale 0 leaves only the "...N more lines" pointers.
func (wd *workData) render(scale float64) string {
	b := func(n int) int { return int(float64(n) * scale) }
	var sections []string
	sections = append(sections, ctxWorkHeader)
	if wd.err != nil {
		return strings.Join(append(sections, "Work state unavailable (read error); try work_state_read"), "\n\n")
	}
	if len(wd.userFacts) > 0 {
		sections = append(sections, ctxTruncate("# Facts (all projects)", wd.userFacts, b(ctxUserBudget), "work_state_read list="+wsUserFactsList, false))
	}
	if len(wd.projFacts) > 0 {
		sections = append(sections, ctxTruncate("# Project facts (durable)", wd.projFacts, b(ctxFactsBudget), "work_state_read list="+wsFactsList, false))
	}
	if len(wd.open) == 0 {
		sections = append(sections, "No open items yet.")
	} else {
		sections = append(sections, ctxTruncate("Still open:", wd.open, b(ctxOpenBudget-utf16Len(ctxWorkHeader)-2), "work_state_read", true))
	}
	return strings.Join(sections, "\n\n")
}

// ctxTruncate keeps as many lines as fit max UTF-16 units, then a pointer line.
// With keepFirst the first line renders even when it alone exceeds max (callers bound line
// length), so a non-empty list never collapses to only the pointer.
func ctxTruncate(head string, lines []string, max int, pointer string, keepFirst bool) string {
	cur := head
	for i, ln := range lines {
		left := len(lines) - i
		more := fmt.Sprintf("\n- ...%d more line%s; %s shows them", left, plural(left), pointer)
		next := cur + "\n" + ln
		extra := 0
		if i < len(lines)-1 {
			extra = utf16Len(more)
		}
		if utf16Len(next)+extra > max && !(keepFirst && i == 0) {
			return cur + more
		}
		cur = next
	}
	return cur
}
