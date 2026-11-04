package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func i64(n int64) *int64 { return &n }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestTurnCostByHand(t *testing.T) {
	// claude-opus-5-5 list prices per MTok: in 4, out 20, 5m write 5, 1h write 8, read 0.20.
	r := costRow{Model: "claude-opus-5-5", In: 1000, Out: 2000, Create: 150000, C5m: i64(100000), C1h: i64(50000), Read: 200000}
	// 1000*4/1e6 + 2000*20/1e6 + 100000*5/1e6 + 50000*8/1e6 + 200000*0.2/1e6
	// = 0.004 + 0.04 + 0.5 + 0.4 + 0.04 = 0.984
	total, write, asRead, ok := turnCost(r)
	if !ok || !near(total, 0.984) || !near(write, 0.9) || !near(asRead, 150000*0.2/1e6) {
		t.Fatalf("total %v write %v asRead %v ok %v, want 0.984 0.9 0.03 true", total, write, asRead, ok)
	}
	// Without the split every written token is priced as a 5-minute write.
	r.C5m, r.C1h = nil, nil
	if total, write, _, _ := turnCost(r); !near(write, 0.75) || !near(total, 0.004+0.04+0.75+0.04) {
		t.Fatalf("unsplit: total %v write %v, want 0.834 0.75", total, write)
	}
}

func TestPriceTable(t *testing.T) {
	for model, want := range map[string]modelPrice{
		"claude-opus-5-5":           {4, 20, 5, 8, 0.20},
		"claude-opus-5-5-20261101":  {4, 20, 5, 8, 0.20}, // a dated id matches by prefix
		"claude-opus-5":             {5, 25, 6.25, 10, 0.50},
		"claude-sonnet-5-5":         {2, 10, 2.5, 4, 0.10},
		"claude-fable-5-1":          {10, 50, 12.5, 20, 0.25},
		"claude-haiku-5-5":          {0.10, 0.50, 0.125, 0.20, 0.01},
		"claude-haiku-4-5-20251001": {1, 5, 1.25, 2, 0.10},
	} {
		got, ok := priceFor(model, 1000)
		if !ok || got != want {
			t.Errorf("%s: %+v ok=%v, want %+v", model, got, ok, want)
		}
	}
	// Haiku 5.5 prompts over 100K tokens use the higher tier.
	if got, _ := priceFor("claude-haiku-5-5", 100001); got != (modelPrice{0.50, 2.50, 0.625, 1, 0.05}) {
		t.Errorf("haiku long tier = %+v", got)
	}
	if _, ok := priceFor("claude-unknown-9", 1); ok {
		t.Error("unknown model priced")
	}
}

var cbase = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

func crow(min int, model string, create int64, split ...int64) costRow {
	r := costRow{Session: "s", Model: model, At: cbase.Add(time.Duration(min) * time.Minute), Create: create}
	if len(split) == 2 {
		r.C5m, r.C1h = i64(split[0]), i64(split[1])
	}
	return r
}

func causes(turns []costTurn) []string {
	var out []string
	for _, t := range turns {
		out = append(out, fmt.Sprintf("%d:%s", int(t.Row.At.Sub(cbase)/time.Minute), t.Cause))
	}
	return out
}

func TestAttributeEachCause(t *testing.T) {
	op, so := "claude-opus-5-5", "claude-sonnet-5-5"
	rows := []costRow{
		crow(0, op, 50000, 50000, 0),   // first turn of the session            -> first/reset
		crow(1, op, 30000, 30000, 0),   // warm cache, same model               -> unexplained
		crow(2, op, 100, 100, 0),       // small write, not reported
		crow(12, op, 40000, 40000, 0),  // 10 min idle, 5m entry expired        -> idle-5m
		crow(13, so, 35000, 35000, 0),  // warm, model changed                  -> model-switch
		crow(14, so, 60000, 0, 60000),  // 1h write, warm, same model           -> unexplained
		crow(44, so, 25000, 0, 25000),  // 30 min idle but the entry is 1h      -> unexplained
		crow(120, so, 26000, 0, 26000), // 76 min idle, 1h entry expired         -> idle-1h
		{Session: "s", Model: so, At: cbase.Add(121 * time.Minute), Create: 27000, Reset: "compact"}, // -> first/reset
		crow(150, so, 28000), // 29 min idle, no split: assumed 5m     -> idle-5m
	}
	got := causes(attributeCache(rows, 20000))
	want := []string{"0:first/reset", "1:unexplained", "12:idle-5m", "13:model-switch", "14:unexplained", "44:unexplained", "120:idle-1h", "121:first/reset", "150:idle-5m"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("causes\n got %v\nwant %v", got, want)
	}
}

func TestAttributeIdleBeatsModelSwitch(t *testing.T) {
	// The entry had expired anyway, so the idle gap is the cause, not the switch.
	rows := []costRow{crow(0, "claude-opus-5-5", 1000, 1000, 0), crow(30, "claude-sonnet-5-5", 40000, 40000, 0)}
	if got := causes(attributeCache(rows, 20000)); strings.Join(got, " ") != "30:idle-5m" {
		t.Fatalf("got %v", got)
	}
}

func TestAttributeChainsAreSeparate(t *testing.T) {
	// A subagent's first turn is its own "first", and it does not give the main thread a
	// previous turn: main turns at 0 and 1 are 1 minute apart however busy the subagent was.
	a := crow(0, "claude-opus-5-5", 50000, 50000, 0)
	side := crow(1, "claude-opus-5-5", 60000, 60000, 0)
	side.Sidechain, side.Agent = true, "a1"
	b := crow(2, "claude-opus-5-5", 30000, 30000, 0)
	got := causes(attributeCache([]costRow{a, side, b}, 20000))
	if strings.Join(got, " ") != "0:first/reset 1:first/reset 2:unexplained" {
		t.Fatalf("got %v", got)
	}
}

func costSeed(t *testing.T, db *sql.DB, id, sess string, min int, model string, side bool, in, out, create, read int64, c5, c1 any) {
	t.Helper()
	ts := cbase.Add(time.Duration(min) * time.Minute).Format("2006-01-02 15:04:05")
	agent := ""
	if side {
		agent = "ag"
	}
	_, err := db.Exec(`INSERT INTO api_usage (message_id, request_id, session_id, project, ts, model, is_sidechain, agent_id,
		input_tokens, output_tokens, cache_creation_input_tokens, cache_creation_5m_input_tokens, cache_creation_1h_input_tokens, cache_read_input_tokens)
		VALUES (?, 'r', ?, 'demo', CAST(? AS TIMESTAMP), ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, sess, ts, model, side, agent, in, out, create, c5, c1, read)
	if err != nil {
		t.Fatal(err)
	}
}

func seedReport(t *testing.T) *sql.DB {
	db, _ := openTestMemd(t)
	op := "claude-opus-5-5"
	costSeed(t, db, "m1", "sess-aaaa1111", 0, op, false, 1000, 2000, 150000, 200000, int64(100000), int64(50000))
	costSeed(t, db, "m2", "sess-aaaa1111", 10, op, false, 10, 20, 40000, 100000, int64(40000), int64(0)) // 10 min later: idle-5m
	costSeed(t, db, "m3", "sess-aaaa1111", 11, op, true, 5, 5, 90000, 0, int64(90000), int64(0))         // subagent
	return db
}

func TestCostReportDefaultExcludesSidechain(t *testing.T) {
	db := seedReport(t)
	out, err := buildCostReport(db, costOpts{Days: 7, MinCache: 20000, Top: 10, Now: cbase.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// m1: 0.984 (by hand above). m2: 10*4 + 20*20 + 40000*5 + 100000*0.2 = 0.00004+0.0004+0.2+0.02 = 0.22044.
	for _, want := range []string{"2 turns", "$1.20", "2026-11-03", "sess-aaa", "(a) idle 10m > 5m", "(c) first turn", "excluded", "1 subagent"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$1.65") {
		t.Errorf("subagent cost leaked into the totals:\n%s", out)
	}
	if n := strings.Count(out, "\n"); n > 40 {
		t.Errorf("report is %d lines, want short", n)
	}
}

func TestCostReportIncludeSidechain(t *testing.T) {
	db := seedReport(t)
	out, err := buildCostReport(db, costOpts{Days: 7, MinCache: 20000, Top: 10, Sidechain: true, Now: cbase.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// m3: 5*4 + 5*20 + 90000*5 = 0.00002+0.0001+0.45 = 0.45012; total 0.984+0.22044+0.45012 = 1.65456
	for _, want := range []string{"3 turns", "$1.65"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "excluded") {
		t.Errorf("nothing is excluded with --include-sidechain:\n%s", out)
	}
}

func TestCostReportDaysWindowKeepsHistoryForAttribution(t *testing.T) {
	db, _ := openTestMemd(t)
	op := "claude-opus-5-5"
	// The previous turn is 3 days old: outside a 1-day window, but it makes the in-window turn
	// an idle expiry rather than a first turn.
	costSeed(t, db, "old", "s1", -3*24*60, op, false, 1, 1, 100, 0, int64(100), int64(0))
	costSeed(t, db, "new", "s1", 0, op, false, 1, 1, 50000, 0, int64(50000), int64(0))
	out, err := buildCostReport(db, costOpts{Days: 1, MinCache: 20000, Top: 10, Now: cbase.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 turns") || !strings.Contains(out, "(a) idle 72h00m > 5m") || strings.Contains(out, "first turn") {
		t.Fatalf("window report:\n%s", out)
	}
}

func TestCostEndpointAndCommand(t *testing.T) {
	db := seedReport(t)
	dir, err := os.MkdirTemp("/tmp", "mc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "m.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: memdHandler(db)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	var out, errw bytes.Buffer
	// cbase is in 2026-11, so a wide window keeps the fixture in range whatever today is.
	if code := memCostTo([]string{"--days", "0"}, sock, &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "2 turns") {
		t.Fatalf("default output:\n%s", out.String())
	}
	out.Reset()
	if code := memCostTo([]string{"--days", "0", "--include-sidechain"}, sock, &out, &errw); code != 0 || !strings.Contains(out.String(), "3 turns") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if code := memCostTo([]string{"--days", "x"}, sock, &out, &errw); code != 2 {
		t.Fatalf("bad flag exit %d", code)
	}
	if code := memCostTo(nil, filepath.Join(dir, "nope.sock"), &out, &errw); code != 1 || !strings.Contains(errw.String(), "memory serve") {
		t.Fatalf("no daemon: exit %d, stderr %q", code, errw.String())
	}
}

func TestPriceOldOpus(t *testing.T) {
	for _, m := range []string{"claude-opus-4-1-20250805", "claude-opus-4-20250514", "claude-opus-4-1"} {
		got, ok := priceFor(m, 1)
		if !ok || got != (modelPrice{15, 75, 18.75, 30, 1.50}) {
			t.Errorf("%s = %+v ok=%v, want the $15/$75 row", m, got, ok)
		}
	}
	for _, m := range []string{"claude-opus-4-5", "claude-opus-4-5-20251101", "claude-opus-4-8", "claude-opus-4-6"} {
		if got, _ := priceFor(m, 1); got != (modelPrice{5, 25, 6.25, 10, 0.50}) {
			t.Errorf("%s = %+v, want the $5/$25 row", m, got)
		}
	}
}

func TestCostDaysOverflowAndServerError(t *testing.T) {
	db := seedReport(t)
	h := memdHandler(db)
	_, all, _ := httpDo(t, h, "GET", "/cost?days=0", "")
	for _, d := range []string{"400000", "9223372036854775807"} {
		code, got, _ := httpDo(t, h, "GET", "/cost?days="+d, "")
		// The header line names the window; compare the rest.
		if code != 200 || strings.SplitN(got, "\n", 2)[1] != strings.SplitN(all, "\n", 2)[1] {
			t.Errorf("days=%s differs from all-time (code %d):\n%s", d, code, got)
		}
	}

	// A bad value is the server's message, not the "is memory serve running?" hint.
	dir, err := os.MkdirTemp("/tmp", "mc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "m.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	var out, errw bytes.Buffer
	if code := memCostTo([]string{"--days=-1"}, sock, &out, &errw); code != 1 ||
		!strings.Contains(errw.String(), "days must be a non-negative integer") || strings.Contains(errw.String(), "memory serve") {
		t.Fatalf("exit %d, stderr %q", code, errw.String())
	}
}
