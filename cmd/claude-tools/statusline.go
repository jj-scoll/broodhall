package main

// statusline (statusLine.command): a status line from the JSON Claude Code pipes in, plus
// prompt-cache state from the transcript. Options (flags, see slConfig) choose the segments,
// the context thresholds, an optional account badge, an optional keep-alive ping counter
// and an optional usage cache file for a desktop widget. With no flags it prints the
// general-purpose line; nothing account- or machine-specific is on by default.
// The drift tripwire is drift.go.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// jget walks object keys like jq's .a.b.c: missing keys and null give nil; indexing
// anything but an object or null is an error.
func jget(v any, keys ...string) (any, bool) {
	for _, k := range keys {
		switch o := v.(type) {
		case nil:
			return nil, true
		case map[string]any:
			v = o[k]
		default:
			return nil, false
		}
	}
	return v, true
}

func jqTruthy(v any) bool { return v != nil && v != false }

// jqText is what `jq -r` prints for a value: strings raw, numbers as written.
func jqText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return "null"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// alt mirrors `jq -r '.a.b // "<def>"'`.
func alt(root any, def string, keys ...string) string {
	if v, ok := jget(root, keys...); ok && jqTruthy(v) {
		return jqText(v)
	}
	return def
}

// jqRank orders values the way jq sorts them: null < false < true < numbers < strings <
// arrays < objects.
func jqRank(v any) int {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 2
		}
		return 1
	case json.Number, float64:
		return 3
	case string:
		return 4
	case []any:
		return 5
	}
	return 6
}

func jqNum(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, _ := t.Float64()
		return f
	case float64:
		return t
	}
	return 0
}

func jqCmp(a, b any) int {
	ra, rb := jqRank(a), jqRank(b)
	if ra != rb {
		return ra - rb
	}
	switch ra {
	case 3:
		x, y := jqNum(a), jqNum(b)
		if x < y {
			return -1
		} else if x > y {
			return 1
		}
		return 0
	case 4:
		return strings.Compare(a.(string), b.(string))
	case 5, 6:
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return bytes.Compare(x, y)
	}
	return 0
}

// orZero is jq's `x // 0`.
func orZero(v any) any {
	if jqTruthy(v) {
		return v
	}
	return json.Number("0")
}

type transcriptEntry struct {
	isSidechain bool
	timestamp   any
	id          any
	usage       map[string]any // nil when .message.usage is null
	content     any
}

// parseEntry decodes one transcript line as jq would read it, or fails where jq errors.
func parseEntry(line []byte) (e transcriptEntry, ok bool) {
	d := json.NewDecoder(bytes.NewReader(line))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil || d.More() {
		return e, false
	}
	sc, ok := jget(v, "isSidechain")
	if !ok {
		return e, false
	}
	e.isSidechain = sc == true
	if e.timestamp, ok = jget(v, "timestamp"); !ok {
		return e, false
	}
	msg, ok := jget(v, "message")
	if !ok {
		return e, false
	}
	if e.id, ok = jget(msg, "id"); !ok {
		return e, false
	}
	if e.content, ok = jget(msg, "content"); !ok {
		return e, false
	}
	u, ok := jget(msg, "usage")
	if !ok {
		return e, false
	}
	switch t := u.(type) {
	case nil:
	case map[string]any:
		e.usage = t
	default:
		e.usage = map[string]any{"!": t} // indexing it errors in jq; caught in cacheStats
	}
	return e, true
}

func decodeAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	_ = d.Decode(&v) // raw already validated by the enclosing Unmarshal
	return v
}

// parseEntryFast is parseEntry without building a generic tree for every field. It splits
// the line into raw top-level and message members (exact-case keys, last duplicate wins,
// like the map decode) and decodes only what the caller needs: wantCache the id, timestamp
// and usage; otherwise content, and only when it is a string that could hold the ping
// marker (a backslash may hide it behind an escape). Anything unusual (invalid JSON, a
// null or non-object line) goes to parseEntry so error behaviour stays identical.
func parseEntryFast(line []byte, wantCache bool, marker []byte) (e transcriptEntry, ok bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(line, &top) != nil || top == nil {
		return parseEntry(line)
	}
	e.isSidechain = string(top["isSidechain"]) == "true"
	var msg map[string]json.RawMessage
	if raw := top["message"]; len(raw) > 0 && string(raw) != "null" {
		if raw[0] != '{' {
			return e, false // jq: cannot index a non-object .message
		}
		if json.Unmarshal(raw, &msg) != nil {
			return parseEntry(line)
		}
	}
	if wantCache {
		if e.isSidechain {
			return e, true // only isSidechain is read for these
		}
		e.timestamp = decodeAny(top["timestamp"])
		e.id = decodeAny(msg["id"])
		switch u := decodeAny(msg["usage"]).(type) {
		case nil:
		case map[string]any:
			e.usage = u
		default:
			e.usage = map[string]any{"!": u}
		}
		return e, true
	}
	if e.isSidechain {
		return e, true
	}
	if c := msg["content"]; len(c) > 0 && c[0] == '"' &&
		(bytes.IndexByte(c, '\\') >= 0 || bytes.Contains(c, marker)) {
		e.content = decodeAny(c)
	}
	return e, true
}

type parsedEntry struct {
	e  transcriptEntry
	ok bool
}

// parseAll runs parseEntryFast over lines on all cores, results in line order.
func parseAll(lines [][]byte, wantCache bool, marker []byte) []parsedEntry {
	out := make([]parsedEntry, len(lines))
	w := runtime.GOMAXPROCS(0)
	if w > len(lines)/64 {
		w = len(lines)/64 + 1
	}
	var wg sync.WaitGroup
	for k := 0; k < w; k++ {
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				out[i].e, out[i].ok = parseEntryFast(lines[i], wantCache, marker)
			}
		}(len(lines)*k/w, len(lines)*(k+1)/w)
	}
	wg.Wait()
	return out
}

var fracZ = regexp.MustCompile(`\.[0-9]+Z$`)

type cacheStats struct {
	last, ttl   int64
	read, write float64
}

// cacheFromTranscript mirrors the jq -s pass over the '"type":"assistant"' lines: main
// thread responses with usage, deduped by message id (unique_by: sorted by id, first of
// each id). Any line jq cannot read, or a value it cannot index, drops the whole result.
func cacheFromTranscript(lines [][]byte) (cacheStats, bool) {
	var s cacheStats
	var es []transcriptEntry
	for _, p := range parseAll(lines, true, nil) {
		e, ok := p.e, p.ok
		if !ok {
			return s, false
		}
		if !e.isSidechain && e.usage != nil {
			if _, bad := e.usage["!"]; bad {
				return s, false
			}
			es = append(es, e)
		}
	}
	sort.SliceStable(es, func(i, j int) bool { return jqCmp(es[i].id, es[j].id) < 0 })
	var u []transcriptEntry
	for i, e := range es {
		if i == 0 || jqCmp(e.id, es[i-1].id) != 0 {
			u = append(u, e)
		}
	}
	if len(u) == 0 {
		return s, false
	}
	// max_by keeps the last of equal maxima.
	maxBy := func(xs []transcriptEntry) *transcriptEntry {
		var m *transcriptEntry
		for i := range xs {
			if m == nil || jqCmp(xs[i].timestamp, m.timestamp) >= 0 {
				m = &xs[i]
			}
		}
		return m
	}
	ts, isStr := maxBy(u).timestamp.(string)
	if !isStr {
		return s, false
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", fracZ.ReplaceAllString(ts, "Z"))
	if err != nil {
		return s, false
	}
	s.last = t.Unix()

	var writes []transcriptEntry
	for _, e := range u {
		if jqCmp(orZero(e.usage["cache_creation_input_tokens"]), json.Number("0")) > 0 {
			writes = append(writes, e)
		}
	}
	s.ttl = 300
	if w := maxBy(writes); w != nil {
		h, ok := jget(w.usage["cache_creation"], "ephemeral_1h_input_tokens")
		if !ok {
			return s, false
		}
		if jqCmp(orZero(h), json.Number("0")) > 0 {
			s.ttl = 3600
		}
	}
	for _, e := range u {
		for _, f := range []struct {
			key string
			sum *float64
		}{{"cache_read_input_tokens", &s.read}, {"cache_creation_input_tokens", &s.write}} {
			v := orZero(e.usage[f.key])
			if jqRank(v) != 3 {
				return s, false // add errors on a non-number
			}
			*f.sum += jqNum(v)
		}
	}
	return s, true
}

// pingsFromTranscript mirrors the jq -s pass over the '"type":"user"' lines: keep-alive
// pings (user entries holding marker) since the last other main-thread user entry.
func pingsFromTranscript(lines [][]byte, marker string) (int, bool) {
	var flags []bool
	for _, p := range parseAll(lines, false, []byte(marker)) {
		e, ok := p.e, p.ok
		if !ok {
			return 0, false
		}
		if e.isSidechain {
			continue
		}
		str, isStr := e.content.(string)
		flags = append(flags, isStr && strings.Contains(str, marker))
	}
	n := 0
	for i := len(flags) - 1; i >= 0 && flags[i]; i-- {
		n++
	}
	return n, true
}

// pingsFromTail is pingsFromTranscript reading back from the end of the file only as far as
// the main-thread user entry that ends the run of pings, in chunks that double, so a long
// transcript costs about one small read. A line it cannot parse there gives 0, as the full
// pass does for one anywhere.
func pingsFromTail(path, marker string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	end, n := st.Size(), 0
	var carry []byte // the first, possibly partial, line of the chunk read last
	for chunk := int64(64 << 10); ; chunk *= 2 {
		start := max(0, end-chunk)
		buf := make([]byte, end-start, end-start+int64(len(carry)))
		if _, err := f.ReadAt(buf, start); err != nil {
			return 0
		}
		lines := bytes.Split(append(buf, carry...), []byte("\n"))
		first := 1
		if start == 0 {
			first = 0
		}
		for i := len(lines) - 1; i >= first; i-- {
			if !bytes.Contains(lines[i], []byte(`"type":"user"`)) {
				continue
			}
			e, ok := parseEntryFast(lines[i], false, []byte(marker))
			if !ok {
				return 0
			}
			if e.isSidechain {
				continue
			}
			if s, isStr := e.content.(string); !isStr || !strings.Contains(s, marker) {
				return n
			}
			n++
		}
		if start == 0 {
			return n
		}
		carry, end = lines[0], start
	}
}

// fmtTok: 1234 -> 1k, 1234567 -> 1.2M (awk's printf, so %d truncates).
func fmtTok(n float64) string {
	if n >= 1e6 {
		return fmt.Sprintf("%.1fM", n/1e6)
	}
	return fmt.Sprintf("%dk", int64((n+500)/1000))
}

// shellInt mirrors `[ "$x" -gt 0 ]`'s integer parse.
func shellInt(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n, err == nil
}

func cmdOut(name string, args ...string) (string, bool) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimRight(string(out), "\n"), err == nil
}

// slSegmentNames is every segment -segments can name, in line order: the identity line
// (account, session, pr, worktree, effort, style) and the main line.
const slSegmentNames = "account,session,pr,worktree,effort,style," +
	"host,user,dir,branch,time,model,elapsed,ctx,cache,ttl,keepalive,totals,reqs,five,reset,week,guards,drift"

// slConfig is the statusline's options, set by flags in the statusLine command.
type slConfig struct {
	segments       map[string]bool // shown segments
	ctxWarn        int64           // tokens: amber from here; 0 = 70% of the window
	ctxLimit       int64           // tokens: red from here, and the denominator; 0 = 90% of the window
	keepMarker     string          // text that marks a keep-alive ping prompt; "" = no ping segment
	keepMax        int             // the N of the ♻n/N ping segment
	accounts       []slAccount     // config-dir base name -> badge label
	accountDefault string          // badge label for any other config dir; "" with no accounts = no badge
	usageCache     string          // file for the desktop widget's usage numbers; "" = none
}

type slAccount struct{ dir, label string }

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// parseSlConfig reads the flags. A bad flag never stops the line: the options parsed so far
// stay, the rest are defaults, and the problem goes to stderr.
func parseSlConfig(args []string) slConfig {
	var c slConfig
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	segs := fs.String("segments", slSegmentNames, "comma-separated segments to show: "+slSegmentNames)
	fs.Int64Var(&c.ctxWarn, "ctx-warn", 0, "context tokens at which ctx turns amber (default 70% of the window)")
	fs.Int64Var(&c.ctxLimit, "ctx-limit", 0, "context tokens at which ctx turns red, shown as the limit (default 90% of the window)")
	fs.StringVar(&c.keepMarker, "keepalive-marker", "", "text in a prompt that marks a cache keep-alive ping; enables the ping counter")
	fs.IntVar(&c.keepMax, "keepalive-max", 4, "the N in the ♻n/N ping counter")
	fs.StringVar(&c.accountDefault, "account-default", "", "account badge for any config dir not named by -account")
	fs.StringVar(&c.usageCache, "usage-cache", "", "write 5h/7d usage to this JSON file for a desktop widget")
	var accts listFlag
	fs.Var(&accts, "account", "account badge: <config-dir-base-name>=<label>, repeatable (CLAUDE_CONFIG_DIR picks it)")
	if err := fs.Parse(args); err != nil {
		c.segments = slSegmentSet(slSegmentNames)
		return c
	}
	c.segments = slSegmentSet(*segs)
	for _, a := range accts {
		if dir, label, ok := strings.Cut(a, "="); ok && dir != "" && label != "" {
			c.accounts = append(c.accounts, slAccount{dir, cleanText(label)})
		}
	}
	c.accountDefault = cleanText(c.accountDefault)
	return c
}

func slSegmentSet(list string) map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Split(list, ",") {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

func statusline(args []string) int {
	cfg := parseSlConfig(args)
	data, _ := io.ReadAll(os.Stdin)
	var in any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if d.Decode(&in) != nil {
		in = nil
	}
	home := os.Getenv("HOME")

	userName := ""
	if u, err := user.Current(); err == nil {
		userName = u.Username
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	now := time.Now().Format("15:04:05")

	// .workspace.current_dir // .cwd // "": an empty string is a value to jq, not a miss.
	workspace := alt(in, "", "cwd")
	if v, ok := jget(in, "workspace", "current_dir"); ok && jqTruthy(v) {
		workspace = jqText(v)
	}
	dir := workspace
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if strings.HasPrefix(dir, home) {
		dir = "~" + dir[len(home):]
	}

	model := alt(in, "", "model", "display_name")
	ctx := alt(in, "", "context_window", "used_percentage")
	ctxTok, ctxSize := "", int64(0)
	if cw, ok := jget(in, "context_window"); ok {
		up, _ := jget(cw, "used_percentage")
		sz, _ := jget(cw, "context_window_size")
		if jqTruthy(up) && jqTruthy(sz) && jqRank(up) == 3 && jqRank(sz) == 3 {
			ctxTok = strconv.FormatFloat(math.Floor(jqNum(up)*jqNum(sz)/100), 'f', -1, 64)
			ctxSize = int64(jqNum(sz))
		}
	}
	five := alt(in, "", "rate_limits", "five_hour", "used_percentage")
	fiveReset := alt(in, "", "rate_limits", "five_hour", "resets_at")
	week := alt(in, "", "rate_limits", "seven_day", "used_percentage")
	weekReset := alt(in, "", "rate_limits", "seven_day", "resets_at")
	durMs := alt(in, "", "cost", "total_duration_ms")
	tpath := alt(in, "", "transcript_path")
	elapsed := ""
	if durMs != "" {
		ms, _ := strconv.ParseFloat(durMs, 64)
		elapsed = fmt.Sprintf("%dm", int64(ms/60000))
	}

	// Prompt-cache hit ratio (cache_read / total_input, most recent API response).
	hit := ""
	cr := alt(in, "0", "context_window", "current_usage", "cache_read_input_tokens")
	ti := alt(in, "0", "context_window", "total_input_tokens")
	if t, ok := shellInt(ti); ok && t > 0 {
		if c, ok := shellInt(cr); ok {
			hit = strconv.FormatInt(100*c/t, 10)
		}
	}

	// Prompt-cache TTL countdown + session cache totals, and keep-alive pings, from the
	// transcript. Main-thread responses only, deduped by message id. The TTL comes from
	// the latest response that wrote cache (1h if it wrote ephemeral_1h tokens, else 5m);
	// expiry is measured from the latest response, since cache reads refresh it too.
	// STATUSLINE_NOW (epoch seconds) overrides the clock for tests.
	// Clients that send prompt_cache (2.1.306+) give the expiry and request/miss counts
	// directly, so only the transcript's tail is read, for the pings.
	var cache cacheStats
	haveCache, pings := false, 0
	pc, havePC := jget(in, "prompt_cache")
	if _, isObj := pc.(map[string]any); !havePC || !isObj {
		havePC = false
	}
	wantPings := cfg.keepMarker != "" && cfg.segments["keepalive"]
	if havePC {
		if isFile(tpath) && wantPings {
			pings = pingsFromTail(tpath, cfg.keepMarker)
		}
	} else if isFile(tpath) {
		if f, err := os.Open(tpath); err == nil {
			var asst, usr [][]byte
			r := bufio.NewReaderSize(f, 1<<20)
			for {
				line, err := r.ReadBytes('\n')
				if len(line) > 0 {
					line = bytes.TrimSuffix(line, []byte("\n"))
					if bytes.Contains(line, []byte(`"type":"assistant"`)) {
						asst = append(asst, line)
					}
					if wantPings && bytes.Contains(line, []byte(`"type":"user"`)) {
						usr = append(usr, line)
					}
				}
				if err != nil {
					break
				}
			}
			f.Close()
			cache, haveCache = cacheFromTranscript(asst)
			pings, _ = pingsFromTranscript(usr, cfg.keepMarker)
		}
	}
	nowSec := time.Now().Unix()
	if v := os.Getenv("STATUSLINE_NOW"); v != "" {
		nowSec, _ = strconv.ParseInt(v, 10, 64)
	}
	cacheLeft, haveLeft := int64(0), false
	if haveCache {
		cacheLeft, haveLeft = cache.last+cache.ttl-nowSec, true
	}
	reqs, misses := "", ""
	if havePC {
		if v, _ := jget(pc, "expires_at"); jqRank(v) == 3 {
			cacheLeft, haveLeft = int64(jqNum(v))-nowSec, true
		}
		if r, _ := jget(pc, "requests"); jqRank(r) == 3 {
			m, _ := jget(pc, "misses")
			reqs, misses = jqText(r), jqText(orZero(m))
		}
	}

	branch := ""
	if workspace != "" {
		if _, ok := cmdOut("git", "-C", workspace, "--no-optional-locks", "rev-parse", "--is-inside-work-tree"); ok {
			branch, _ = cmdOut("git", "-C", workspace, "--no-optional-locks", "branch", "--show-current")
		}
	}

	// Line 2 is a list of segments; a narrow $COLUMNS drops the highest prio numbers first
	// (0 is never dropped).
	var segs []slSeg
	add := func(name string, prio int, format string, a ...any) {
		if cfg.segments[name] {
			segs = append(segs, slSeg{fmt.Sprintf(format, a...), prio})
		}
	}
	add("host", 7, "\033[38;5;128m[%s]\033[0m", host)
	add("user", 6, "\033[38;5;253m%s\033[0m \033[38;5;102min\033[0m", userName)
	add("dir", 0, "\033[1;38;5;228m%s\033[0m", dir)
	if branch != "" {
		add("branch", 2, "\033[38;5;102mon\033[0m \033[38;5;159m %s\033[0m", branch)
	}
	add("time", 5, "\033[38;5;254m%s\033[0m", now)
	if model != "" {
		add("model", 0, "\033[38;5;102m|\033[0m \033[38;5;123m%s\033[0m", model)
	}
	if elapsed != "" {
		add("elapsed", 8, "\033[38;5;244m⏱%s\033[0m", elapsed)
	}
	// ctx tokens against the warn and limit thresholds: green below warn, amber from warn,
	// red from limit. Unset thresholds are 70% and 90% of the window the payload reports.
	if t, ok := shellInt(ctxTok); ok {
		warn, limit := cfg.ctxWarn, cfg.ctxLimit
		if limit == 0 {
			limit = ctxSize * 9 / 10
		}
		if warn == 0 {
			warn = ctxSize * 7 / 10
		}
		col := 82
		if limit > 0 && t >= limit {
			col = 196
		} else if warn > 0 && t >= warn {
			col = 214
		}
		if limit > 0 {
			add("ctx", 1, "\033[38;5;%dmctx %dk/%dk\033[0m", col, (t+500)/1000, (limit+500)/1000)
		} else {
			add("ctx", 1, "\033[38;5;%dmctx %dk\033[0m", col, (t+500)/1000)
		}
	}
	// cache hit %: green when warm (cheap reads), red when cold
	if h, ok := shellInt(hit); ok {
		col := 203
		if h >= 70 {
			col = 82
		} else if h >= 40 {
			col = 220
		}
		add("cache", 4, "\033[38;5;%dm⚡cache:%d%%\033[0m", col, h)
	}
	// cache TTL: green with time left, amber in the last 5 minutes, red once cold
	if haveLeft {
		switch {
		case cacheLeft <= 0:
			add("ttl", 2, "\033[1;38;5;196m⏳cold\033[0m")
		case cacheLeft < 300:
			add("ttl", 2, "\033[38;5;214m⏳%dm\033[0m", (cacheLeft+59)/60)
		default:
			add("ttl", 2, "\033[38;5;82m⏳%dm\033[0m", (cacheLeft+59)/60)
		}
		// Close to expiring (the amber window, or already cold): say what the next prompt
		// will cost to rebuild, when the payload says.
		if v, _ := jget(pc, "recache_tokens_if_cold"); cacheLeft < 300 && jqRank(v) == 3 && jqNum(v) > 0 {
			add("ttl", 1, "\033[38;5;214mcold = %s recache\033[0m", fmtTok(jqNum(v)))
		}
	}
	if pings > 0 {
		add("keepalive", 7, "\033[38;5;117m♻%d/%d\033[0m", pings, cfg.keepMax)
	}
	if haveCache {
		add("totals", 8, "\033[38;5;244mΣ%s rd %s wr\033[0m", fmtTok(cache.read), fmtTok(cache.write))
	}
	// requests and misses: a miss is a rebuild, so its count goes red
	if reqs != "" {
		col := 244
		if misses != "0" {
			col = 196
		}
		add("reqs", 6, "\033[38;5;244m%sreq\033[0m \033[38;5;%dm%smiss\033[0m", reqs, col, misses)
	}
	if five != "" {
		f, _ := strconv.ParseFloat(five, 64)
		add("five", 0, "\033[1;38;5;214m⬆ 5h:%.0f%%\033[0m", f)
		if left := resetsIn(fiveReset, nowSec); left != "" {
			add("reset", 3, "\033[38;5;214m↻%s\033[0m", left)
		}
	}
	if week != "" {
		f, _ := strconv.ParseFloat(week, 64)
		add("week", 4, "\033[38;5;222m7d:%.0f%%\033[0m", f)
	}
	if g := guardSummary(alt(in, "", "session_id")); g != "" {
		add("guards", 3, "\033[38;5;245m%s\033[0m", g)
	}
	// drift tripwire: circular tool use in the last 60 calls (never fails the line)
	if isFile(tpath) && cfg.segments["drift"] {
		if drift := driftVerdict(tpath); drift != "" {
			add("drift", 0, "\033[1;38;5;196m⚠ %s\033[0m", drift)
		}
	}
	cols, _ := strconv.Atoi(os.Getenv("COLUMNS"))
	if l1 := identityLine(in, cols, cfg); l1 != "" {
		os.Stdout.WriteString(l1 + "\n")
	}
	os.Stdout.WriteString(fitSegments(segs, cols) + "\n")

	if cfg.usageCache != "" && five+week != "" {
		writeUsageCache(cfg.usageCache, ctx, five, fiveReset, week, weekReset)
	}
	return 0
}

// slSeg is one piece of a status line. prio orders what a narrow terminal drops: the highest
// number goes first, ties from the right; 0 is never dropped.
type slSeg struct {
	text string
	prio int
}

// visibleWidth is the terminal cells a string takes: escapes (SGR, OSC 8 links) are free and
// the emoji-width glyphs the line uses count 2.
func visibleWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) {
			switch s[i+1] {
			case '[': // CSI: up to the final byte
				i += 2
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
				i++
				continue
			case ']': // OSC: up to BEL or ST
				i += 2
				for i < len(s) && s[i] != 0x07 && s[i] != 0x1b {
					i++
				}
				if i < len(s) && s[i] == 0x1b {
					i++
				}
				i++
				continue
			}
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		w++
		if r == '⚡' || r == '⏳' || r == '⏱' {
			w++
		}
	}
	return w
}

// fitSegments joins segs with spaces, first dropping the lowest-priority ones until the
// line fits cols (cols <= 0: no limit).
func fitSegments(segs []slSeg, cols int) string {
	for cols > 0 {
		total, drop := len(segs)-1, -1
		for i, s := range segs {
			total += visibleWidth(s.text)
			if s.prio > 0 && (drop < 0 || s.prio >= segs[drop].prio) {
				drop = i
			}
		}
		if total <= cols || drop < 0 {
			break
		}
		segs = append(segs[:drop:drop], segs[drop+1:]...)
	}
	texts := make([]string, len(segs))
	for i, s := range segs {
		texts[i] = s.text
	}
	return strings.Join(texts, " ")
}

// cleanText drops control characters, so payload text can't inject terminal escapes.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// jstr is a string field, or "" when it is missing or not a string.
func jstr(in any, keys ...string) string {
	v, _ := jget(in, keys...)
	s, _ := v.(string)
	return cleanText(s)
}

// identityLine is line 1: which account, session, effort, worktree, PR and style this is.
// Each field shows only when the payload has it (and, for effort and style, when it isn't the
// default).
func identityLine(in any, cols int, cfg slConfig) string {
	var segs []slSeg
	add := func(name string, text string, prio int) {
		if cfg.segments[name] {
			segs = append(segs, slSeg{text, prio})
		}
	}
	if badge := accountBadge(cfg, os.Getenv("CLAUDE_CONFIG_DIR")); badge != "" {
		add("account", badge, 0)
	}
	if n := []rune(jstr(in, "session_name")); len(n) > 0 {
		if len(n) > 40 {
			n = append(n[:39], '…')
		}
		add("session", "\033[38;5;253m"+string(n)+"\033[0m", 1)
	}
	if pr, _ := jget(in, "pr", "number"); jqRank(pr) == 3 {
		text := "\033[38;5;111mPR #" + jqText(pr) + "\033[0m"
		if u := jstr(in, "pr", "url"); strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
			text = "\033]8;;" + u + "\033\\" + text + "\033]8;;\033\\"
		}
		add("pr", text, 2)
	}
	wt := jstr(in, "worktree", "name")
	if wt == "" {
		wt = jstr(in, "workspace", "git_worktree")
	}
	if wt != "" {
		add("worktree", "\033[38;5;102mwt:\033[0m\033[38;5;159m"+wt+"\033[0m", 3)
	}
	if e := jstr(in, "effort", "level"); e != "" && e != "medium" {
		add("effort", "\033[38;5;179meffort:"+e+"\033[0m", 4)
	}
	if st := jstr(in, "output_style", "name"); st != "" && st != "default" {
		add("style", "\033[38;5;102mstyle:"+st+"\033[0m", 5)
	}
	return fitSegments(segs, cols)
}

// accountBadge is the badge for CLAUDE_CONFIG_DIR: the label of the -account whose directory
// base name matches, else -account-default, else "" (no badge).
func accountBadge(cfg slConfig, configDir string) string {
	base := filepath.Base(filepath.Clean(configDir))
	if configDir == "" {
		base = ""
	}
	for _, a := range cfg.accounts {
		if a.dir == base {
			return "\033[1;38;5;208m" + a.label + "\033[0m"
		}
	}
	if cfg.accountDefault != "" {
		return "\033[38;5;39m" + cfg.accountDefault + "\033[0m"
	}
	return ""
}

// resetsIn is the time left until an epoch-seconds reset: "1h23m", "45m"; "" when unknown
// or already past.
func resetsIn(resetsAt string, now int64) string {
	at, err := strconv.ParseInt(resetsAt, 10, 64)
	if err != nil || at <= now {
		return ""
	}
	m := (at - now + 59) / 60
	if m >= 60 {
		return fmt.Sprintf("%dh%02dm", m/60, m%60)
	}
	return fmt.Sprintf("%dm", m)
}

// writeUsageCache keeps the usage file a desktop widget reads (-usage-cache). Each session
// reports the limits as of its own last reply, so an idle one repeats older, lower numbers:
// within a window keep the highest, and never go back to an earlier window. Written to a
// temp file and moved into place, so the widget never reads it half-written.
func writeUsageCache(f, ctx, five, fiveReset, week, weekReset string) {
	dir := filepath.Dir(f)
	os.MkdirAll(dir, 0o755)
	or := func(s string) string {
		if s == "" {
			return "null"
		}
		return s
	}
	newText := fmt.Sprintf(`{"ctx":%s,"five":%s,"five_reset":%s,"week":%s,"week_reset":%s}`,
		or(ctx), or(five), or(fiveReset), or(week), or(weekReset))
	// ~20 sessions merge into the same file: hold a lock on a sidecar (the data file itself
	// is replaced by the rename, so it cannot carry the lock) across read, merge and move.
	// Without the lock (or when a writer stuck in a disk stall holds it past a second) the
	// best-effort write still happens, as before.
	if lk, err := os.OpenFile(f+".lock", os.O_CREATE|os.O_RDWR, 0o644); err == nil {
		defer lk.Close()
		if lockWithin(lk, time.Second) {
			sweepUsageOrphans(lk, f)
		}
	}
	out := mergeUsage(f, newText)
	tmp := fmt.Sprintf("%s.%d", f, os.Getpid())
	if err := os.WriteFile(tmp, []byte(out), 0o644); err != nil || os.Rename(tmp, f) != nil {
		os.Remove(tmp)
	}
}

// lockWithin takes an exclusive flock on f, giving up after d so one stalled holder does
// not queue every session's statusline behind it.
func lockWithin(f *os.File, d time.Duration) bool {
	for deadline := time.Now().Add(d); ; time.Sleep(10 * time.Millisecond) {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// sweepUsageOrphans removes <file>.<pid> temp files a killed writer left
// behind (statusline timeout, session teardown between create and rename). The lock
// file's mtime paces it to once per usageSweepEvery, so the 30 s render stays one stat.
// Called with the lock held; a live writer's temp file is seconds old, never this stale.
func sweepUsageOrphans(lk *os.File, f string) {
	const usageSweepEvery = 10 * time.Minute
	st, err := lk.Stat()
	if err != nil || (st.Size() > 0 && time.Since(st.ModTime()) < usageSweepEvery) {
		return
	}
	now := time.Now()
	lk.WriteAt([]byte("x"), 0) // size > 0 marks a sweep done; the write bumps mtime
	m, _ := filepath.Glob(f + ".[0-9]*")
	for _, p := range m {
		if fi, err := os.Stat(p); err == nil && now.Sub(fi.ModTime()) > usageSweepEvery {
			os.Remove(p)
		}
	}
}

// mergeUsage mirrors the jq merge over the old file, falling back to the new values alone
// wherever jq fails (no file, bad JSON, a value it cannot index).
func mergeUsage(path, newText string) string {
	fallback := newText + "\n"
	var n map[string]any
	nd := json.NewDecoder(strings.NewReader(newText))
	nd.UseNumber()
	if nd.Decode(&n) != nil || nd.More() {
		return fallback
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	var out strings.Builder
	od := json.NewDecoder(bytes.NewReader(data))
	od.UseNumber()
	for {
		var old any
		if err := od.Decode(&old); err != nil {
			if err == io.EOF {
				if out.Len() == 0 {
					return fallback // an empty file would otherwise stay empty for good
				}
				return out.String() // one line per input value, as jq prints
			}
			return fallback
		}
		om, isObj := old.(map[string]any)
		if !isObj && old != nil {
			return fallback
		}
		get := func(m map[string]any, k string) any { return m[k] }
		res := map[string]any{"ctx": n["ctx"]}
		for _, k := range [][2]string{{"five", "five_reset"}, {"week", "week_reset"}} {
			p, r := k[0], k[1]
			switch c := jqCmp(orZero(get(om, r)), orZero(n[r])); {
			case c > 0:
				res[p], res[r] = get(om, p), get(om, r)
			case c < 0:
				res[p], res[r] = n[p], n[r]
			default:
				res[p], res[r] = n[p], n[r]
				if jqCmp(get(om, p), n[p]) > 0 { // max keeps the last of equals
					res[p] = get(om, p)
				}
			}
		}
		out.WriteString("{")
		for i, k := range []string{"ctx", "five", "five_reset", "week", "week_reset"} {
			if i > 0 {
				out.WriteString(",")
			}
			kb, _ := json.Marshal(k)
			out.Write(kb)
			out.WriteString(":")
			out.WriteString(jqCompact(res[k]))
		}
		out.WriteString("}\n")
	}
}

// jqCompact prints a value as `jq -c` does, keeping number literals as written.
func jqCompact(v any) string {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}
