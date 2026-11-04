package main

import (
	"bytes"
	"os"
)

// Remote Control has no field in the statusLine payload. While it is on, Claude Code keeps
// rewriting a {"type":"bridge-session",...} line into the session transcript, about every 20
// to 40 entries (99.9% of gaps in a sample of 27,000 real lines are 44 entries or fewer, the
// largest 258). When it is turned off the lines stop and the session carries on. So the rule
// is: Remote Control is on if a bridge-session line is among the last rcMaxLinesAfter lines.
// Counting entries rather than time keeps an idle but connected session lit, since an idle
// session writes nothing.
const (
	rcMarker        = `{"type":"bridge-session"`
	rcMaxLinesAfter = 100
	// rcScanCap bounds the tail read. A marker further back than this is treated as stale.
	// The largest gap between two real bridge lines was 1.7 MB.
	rcScanCap = 4 << 20
)

// remoteControlActive reports whether the transcript at path shows Remote Control on now. It
// reads the file backwards in chunks that double, so a long transcript costs one small read.
// An unreadable or missing file is not on.
func remoteControlActive(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	end, after := st.Size(), 0
	floor := max(0, end-rcScanCap)
	var carry []byte // the first, possibly partial, line of the chunk read last
	for chunk := int64(64 << 10); end > floor; chunk *= 2 {
		start := max(floor, end-chunk)
		buf := make([]byte, end-start, end-start+int64(len(carry)))
		if _, err := f.ReadAt(buf, start); err != nil {
			return false
		}
		lines := bytes.Split(append(buf, carry...), []byte("\n"))
		first := 1 // lines[0] is cut off at the chunk start unless the chunk reaches the file start
		if start == 0 {
			first = 0
		}
		for i := len(lines) - 1; i >= first; i-- {
			if len(lines[i]) == 0 {
				continue
			}
			if bytes.HasPrefix(lines[i], []byte(rcMarker)) {
				return after <= rcMaxLinesAfter
			}
			if after++; after > rcMaxLinesAfter {
				return false
			}
		}
		carry, end = lines[0], start
	}
	return false
}
