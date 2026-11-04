package main

// `memory cost`: what the transcripts' token usage costs at API list prices, and why the big
// cache writes happened. The daemon owns the database, so the subcommand asks it over the
// socket (GET /cost) and prints the text it returns.

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type costRow struct {
	Session, Agent, Project, Model, Reset string
	At                                    time.Time
	Sidechain                             bool // subagent or observer: not part of the main thread
	In, Out, Create, Read                 int64
	C5m, C1h                              *int64 // nil: the transcript has no 5m/1h split
}

// costTurn is a large cache write and the cause attributed to it.
type costTurn struct {
	Row       costRow
	Cause     string
	Gap       time.Duration // since the previous response in the same chain
	PrevModel string
}

const (
	causeFirst  = "first/reset"
	causeIdle5m = "idle-5m"
	causeIdle1h = "idle-1h"
	causeModel  = "model-switch"
	causeOther  = "unexplained"
)

// costCauseOrder is the report order, with the label shown for each.
var costCauseOrder = []struct{ key, label string }{
	{causeIdle5m, "(a) idle > 5m"},
	{causeIdle1h, "(a) idle > 1h"},
	{causeModel, "(b) model switch"},
	{causeFirst, "(c) first/clear/compact"},
	{causeOther, "(d) unexplained"},
}

// costMaxDays: a larger window means all time.
const costMaxDays = 36500

type costOpts struct {
	Days      int // 0: all time
	Sidechain bool
	MinCache  int64 // a cache write of at least this many tokens is "large"
	Top       int
	Now       time.Time
}

// modelPrice is USD per million tokens.
type modelPrice struct{ In, Out, W5m, W1h, Read float64 }

// costPricesDate and costPricesSource say where costPrices came from. Standard (global)
// prices for the Claude API; 5-minute writes are 1.25x input, 1-hour writes 2x. Update the
// table and the date together.
const (
	costPricesDate   = "2026-11-04"
	costPricesSource = "https://platform.claude.com/docs/en/about-claude/pricing"
)

// costPrices is matched by longest model-id prefix, so a dated id (…-20251001) finds its row.
var costPrices = map[string]modelPrice{
	"claude-fable-5-1":  {10, 50, 12.5, 20, 0.25},
	"claude-mythos-5-1": {10, 50, 12.5, 20, 0.25},
	"claude-fable-5":    {10, 50, 12.5, 20, 1},
	"claude-mythos-5":   {10, 50, 12.5, 20, 1},
	"claude-opus-5-5":   {4, 20, 5, 8, 0.20},
	"claude-opus-5":     {5, 25, 6.25, 10, 0.50},
	"claude-opus-4-5":   {5, 25, 6.25, 10, 0.50},
	"claude-opus-4-6":   {5, 25, 6.25, 10, 0.50},
	"claude-opus-4-7":   {5, 25, 6.25, 10, 0.50},
	"claude-opus-4-8":   {5, 25, 6.25, 10, 0.50},
	// Opus 4.1 and the original Opus 4 (dated ids like claude-opus-4-20250514) cost 3x as much.
	// The page lists 4.1 and says earlier releases keep their pricing, so Opus 4 uses the same row.
	"claude-opus-4-1":    {15, 75, 18.75, 30, 1.50},
	"claude-opus-4-2025": {15, 75, 18.75, 30, 1.50},
	"claude-sonnet-5-5":  {2, 10, 2.5, 4, 0.10},
	"claude-sonnet-5":    {2, 10, 2.5, 4, 0.20},
	"claude-sonnet-4":    {3, 15, 3.75, 6, 0.30},
	"claude-haiku-5-5":   {0.10, 0.50, 0.125, 0.20, 0.01},
	"claude-haiku-4-5":   {1, 5, 1.25, 2, 0.10},
}

// haikuLongPrice is Claude Haiku 5.5 for prompts over 100,000 tokens.
var haikuLongPrice = modelPrice{0.50, 2.50, 0.625, 1, 0.05}

// priceFor returns the list price for model; promptTokens (input + cache writes + cache
// reads) selects Haiku 5.5's long-prompt tier.
func priceFor(model string, promptTokens int64) (modelPrice, bool) {
	best := ""
	for k := range costPrices {
		if strings.HasPrefix(model, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return modelPrice{}, false
	}
	if best == "claude-haiku-5-5" && promptTokens > 100000 {
		return haikuLongPrice, true
	}
	return costPrices[best], true
}

// turnCost is the list-price cost of one response; write is the part spent on cache writes
// and asRead what those tokens would have cost as cache reads. Written tokens without a
// 5m/1h split are priced as 5-minute writes. priced is false for a model not in costPrices
// (cost 0).
func turnCost(r costRow) (total, write, asRead float64, priced bool) {
	p, ok := priceFor(r.Model, r.In+r.Create+r.Read)
	if !ok {
		return 0, 0, 0, false
	}
	var w5, w1 int64
	if r.C1h != nil {
		w1 = *r.C1h
	}
	w5 = r.Create - w1
	if r.C5m != nil && *r.C5m > w5 {
		w5 = *r.C5m
	}
	if w5 < 0 {
		w5 = 0
	}
	const m = 1e6
	write = float64(w5)*p.W5m/m + float64(w1)*p.W1h/m
	asRead = float64(r.Create) * p.Read / m
	total = float64(r.In)*p.In/m + float64(r.Out)*p.Out/m + float64(r.Read)*p.Read/m + write
	return total, write, asRead, true
}

// attributeCache finds the responses that wrote at least minCache tokens to the cache and
// names a likely cause for each. Responses are grouped into chains (the main thread of a
// session, or one subagent); within a chain, in time order, the first matching rule wins:
//
//	first/reset   no earlier response in the chain, or the first one after /clear or compaction
//	idle-5m/1h    the gap since the previous response is longer than that entry's lifetime: 1h
//	              if the previous write was all 1-hour (per the 5m/1h split), else 5m (also the
//	              assumption when the transcript has no split); a read keeps the lifetime
//	model-switch  the model differs from the previous response (the cache is per model)
//	unexplained   none of the above: warm cache, same model, so the write is new content
//
// Idle ranks above a model switch because the entry had expired either way. Transcript
// timestamps are when a block was written, not when the request started, so gaps near a
// lifetime boundary are approximate.
func attributeCache(rows []costRow, minCache int64) []costTurn {
	chains := map[string][]costRow{}
	for _, r := range rows {
		k := r.Session
		if r.Sidechain {
			k += "\x00side\x00" + r.Agent
		}
		chains[k] = append(chains[k], r)
	}
	var out []costTurn
	for _, c := range chains {
		sort.SliceStable(c, func(i, j int) bool { return c[i].At.Before(c[j].At) })
		ttl := 5 * time.Minute
		for i, r := range c {
			if r.Create >= minCache {
				t := costTurn{Row: r}
				if i > 0 {
					t.Gap, t.PrevModel = r.At.Sub(c[i-1].At), c[i-1].Model
				}
				switch {
				case i == 0 || r.Reset != "":
					t.Cause = causeFirst
				case t.Gap > ttl && ttl == time.Hour:
					t.Cause = causeIdle1h
				case t.Gap > ttl:
					t.Cause = causeIdle5m
				case r.Model != t.PrevModel:
					t.Cause = causeModel
				default:
					t.Cause = causeOther
				}
				out = append(out, t)
			}
			if r.Create > 0 {
				ttl = 5 * time.Minute
				if r.C1h != nil && *r.C1h > 0 && (r.C5m == nil || *r.C5m == 0) {
					ttl = time.Hour
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Row.At.Equal(out[j].Row.At) {
			return out[i].Row.At.Before(out[j].Row.At)
		}
		return out[i].Row.Session < out[j].Row.Session
	})
	return out
}

// loadCostRows reads every response of every session that has one in the window, so a
// response just inside the window still sees the one before it.
func loadCostRows(db *sql.DB, since time.Time) ([]costRow, error) {
	q := `SELECT session_id, agent_id, project, model, reset_kind, ts, (is_sidechain OR is_observer),
  input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
  cache_creation_5m_input_tokens, cache_creation_1h_input_tokens FROM api_usage`
	var args []any
	if !since.IsZero() {
		q += ` WHERE session_id IN (SELECT session_id FROM api_usage WHERE ts >= CAST(? AS TIMESTAMP))`
		args = append(args, memdTS(since))
	}
	rs, err := db.Query(q+` ORDER BY session_id, ts, message_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("usage query: %w", err)
	}
	defer rs.Close()
	var rows []costRow
	for rs.Next() {
		var r costRow
		var c5, c1 sql.NullInt64
		if err := rs.Scan(&r.Session, &r.Agent, &r.Project, &r.Model, &r.Reset, &r.At, &r.Sidechain,
			&r.In, &r.Out, &r.Create, &r.Read, &c5, &c1); err != nil {
			return nil, err
		}
		if c5.Valid {
			r.C5m = &c5.Int64
		}
		if c1.Valid {
			r.C1h = &c1.Int64
		}
		r.At = r.At.UTC()
		rows = append(rows, r)
	}
	return rows, rs.Err()
}

type costAgg struct {
	turns          int
	tokens         int64
	usd, writeUSD  float64
	project, label string
}

func humanTok(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}

func shortModel(m string) string { return strings.TrimPrefix(m, "claude-") }

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func humanDur(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

// buildCostReport renders the report as plain text.
func buildCostReport(db *sql.DB, o costOpts) (string, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Days > costMaxDays { // beyond any history, and time.Duration overflows near 106,000 days
		o.Days = 0
	}
	var start time.Time
	if o.Days > 0 {
		start = o.Now.UTC().Add(-time.Duration(o.Days) * 24 * time.Hour)
	}
	rows, err := loadCostRows(db, start)
	if err != nil {
		return "", err
	}
	inScope := func(r costRow) bool { return (start.IsZero() || !r.At.Before(start)) && (o.Sidechain || !r.Sidechain) }

	var total costAgg
	var inTok, outTok, wTok, rTok int64
	days, sessions := map[string]*costAgg{}, map[string]*costAgg{}
	exclTurns, exclUSD := 0, 0.0
	unpriced := map[string]bool{}
	add := func(m map[string]*costAgg, k string, r costRow, usd, w float64, tok int64) {
		a := m[k]
		if a == nil {
			a = &costAgg{project: r.Project}
			m[k] = a
		}
		a.turns++
		a.tokens += tok
		a.usd += usd
		a.writeUSD += w
	}
	for _, r := range rows {
		if !start.IsZero() && r.At.Before(start) {
			continue
		}
		usd, w, _, ok := turnCost(r)
		if !ok {
			unpriced[r.Model] = true
		}
		if !inScope(r) {
			exclTurns++
			exclUSD += usd
			continue
		}
		tok := r.In + r.Out + r.Create + r.Read
		total.turns++
		total.tokens += tok
		total.usd += usd
		total.writeUSD += w
		inTok, outTok, wTok, rTok = inTok+r.In, outTok+r.Out, wTok+r.Create, rTok+r.Read
		add(days, r.At.Format("2006-01-02"), r, usd, w, tok)
		add(sessions, r.Session, r, usd, w, tok)
	}

	var b strings.Builder
	window := "all time"
	if o.Days > 0 {
		window = fmt.Sprintf("last %d days", o.Days)
	}
	fmt.Fprintf(&b, "cost at API list prices, %s (UTC); prices dated %s, %s\n", window, costPricesDate, costPricesSource)
	if o.Sidechain {
		b.WriteString("scope: main thread + subagents/observer\n")
	} else {
		fmt.Fprintf(&b, "scope: main thread only; excluded %d subagent/observer turns, $%.2f (--include-sidechain to add)\n", exclTurns, exclUSD)
	}
	fmt.Fprintf(&b, "total: %d turns, %s tokens (in %s, out %s, cache write %s, cache read %s), $%.2f, of which cache writes $%.2f\n",
		total.turns, humanTok(total.tokens), humanTok(inTok), humanTok(outTok), humanTok(wTok), humanTok(rTok), total.usd, total.writeUSD)
	if len(unpriced) > 0 {
		var ms []string
		for m := range unpriced {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		fmt.Fprintf(&b, "unpriced models, counted as $0: %s\n", strings.Join(ms, ", "))
	}

	b.WriteString("\nper day\n")
	var dkeys []string
	for k := range days {
		dkeys = append(dkeys, k)
	}
	sort.Strings(dkeys)
	for _, k := range dkeys {
		a := days[k]
		fmt.Fprintf(&b, "  %s  %5d turns  %7s tok  $%8.2f  cache writes $%.2f\n", k, a.turns, humanTok(a.tokens), a.usd, a.writeUSD)
	}

	top := o.Top
	if top <= 0 {
		top = 10
	}
	var skeys []string
	for k := range sessions {
		skeys = append(skeys, k)
	}
	sort.Slice(skeys, func(i, j int) bool {
		if sessions[skeys[i]].usd != sessions[skeys[j]].usd {
			return sessions[skeys[i]].usd > sessions[skeys[j]].usd
		}
		return skeys[i] < skeys[j]
	})
	fmt.Fprintf(&b, "\nper session (top %d of %d by cost)\n", min(top, len(skeys)), len(skeys))
	for _, k := range skeys[:min(top, len(skeys))] {
		a := sessions[k]
		fmt.Fprintf(&b, "  %-8s  %5d turns  %7s tok  $%8.2f  cache writes $%-7.2f %s\n", shortID(k), a.turns, humanTok(a.tokens), a.usd, a.writeUSD, a.project)
	}

	turns := attributeCache(rows, o.MinCache)
	type causeAgg struct {
		n          int
		tok        int64
		write, ext float64
	}
	byCause := map[string]*causeAgg{}
	var shown []costTurn
	for _, t := range turns {
		if !inScope(t.Row) {
			continue
		}
		_, w, asRead, _ := turnCost(t.Row)
		a := byCause[t.Cause]
		if a == nil {
			a = &causeAgg{}
			byCause[t.Cause] = a
		}
		a.n++
		a.tok += t.Row.Create
		a.write += w
		a.ext += w - asRead
		shown = append(shown, t)
	}
	fmt.Fprintf(&b, "\ncache writes of >= %s tokens, by likely cause (extra = write cost minus the same tokens as cache reads)\n", humanTok(o.MinCache))
	var sumN int
	var sumW float64
	for _, c := range costCauseOrder {
		a := byCause[c.key]
		if a == nil {
			a = &causeAgg{}
		}
		sumN += a.n
		sumW += a.write
		fmt.Fprintf(&b, "  %-24s %5d turns  %7s tok  write $%-8.2f extra $%.2f\n", c.label, a.n, humanTok(a.tok), a.write, a.ext)
	}
	fmt.Fprintf(&b, "  %-24s %5d turns  of %d total; $%.2f of $%.2f cache-write spend\n", "all large", sumN, total.turns, sumW, total.writeUSD)

	sort.SliceStable(shown, func(i, j int) bool {
		_, wi, _, _ := turnCost(shown[i].Row)
		_, wj, _, _ := turnCost(shown[j].Row)
		return wi > wj
	})
	if len(shown) > 0 {
		fmt.Fprintf(&b, "\nlargest cache writes (top %d)\n", min(top, len(shown)))
	}
	for _, t := range shown[:min(top, len(shown))] {
		_, w, _, _ := turnCost(t.Row)
		fmt.Fprintf(&b, "  %s  %-8s  %-14s %6s tok  $%-6.2f %s\n", t.Row.At.Format("01-02 15:04"), shortID(t.Row.Session),
			shortModel(t.Row.Model), humanTok(t.Row.Create), w, costWhy(t))
	}
	return b.String(), nil
}

func costWhy(t costTurn) string {
	switch t.Cause {
	case causeFirst:
		switch t.Row.Reset {
		case "clear":
			return "(c) after /clear"
		case "compact":
			return "(c) after compaction"
		}
		return "(c) first turn"
	case causeIdle5m:
		return "(a) idle " + humanDur(t.Gap) + " > 5m"
	case causeIdle1h:
		return "(a) idle " + humanDur(t.Gap) + " > 1h"
	case causeModel:
		return "(b) model " + shortModel(t.PrevModel) + " -> " + shortModel(t.Row.Model)
	}
	return "(d) unexplained"
}

// memCostTo is `memory cost`: parse flags, ask the daemon at sock for the report, print it.
// It exits 2 on bad flags and 1 when the daemon cannot be reached.
func memCostTo(args []string, sock string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("memory cost", flag.ContinueOnError)
	fs.SetOutput(errw)
	days := fs.Int("days", 7, "window in days; 0 for all time")
	side := fs.Bool("include-sidechain", false, "include subagent and observer turns")
	minCache := fs.Int64("min-cache", 20000, "cache-write tokens that make a turn large")
	top := fs.Int("top", 10, "rows in the session and largest-turn lists")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := url.Values{}
	q.Set("days", strconv.Itoa(*days))
	q.Set("min_cache", strconv.FormatInt(*minCache, 10))
	q.Set("top", strconv.Itoa(*top))
	if *side {
		q.Set("include_sidechain", "1")
	}
	status, body, err := memGetStatus(sock, "/cost?"+q.Encode(), 30*time.Second)
	if err != nil {
		fmt.Fprintf(errw, "memory cost: %v (is `memory serve` running?)\n", err)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(errw, "memory cost: %s\n", strings.TrimSpace(string(body)))
		return 1
	}
	out.Write(body)
	return 0
}
