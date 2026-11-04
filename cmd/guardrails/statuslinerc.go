package main

import (
	"bytes"
	"os"
)

// Remote Control has no field in the statusLine payload, so this reads the session transcript.
//
// Claude Code writes a {"type":"bridge-session",...} line with each re-write of the session
// metadata (every 20 to 40 entries, 99.9% of gaps in a sample of 27,000 real lines are 44 or
// fewer, the largest 258). It is not a heartbeat: the lines go on after Remote Control
// disconnects, usually with the same id. The rule is therefore two parts:
//
//   - a bridge-session line must be among the last rcMaxLinesAfter lines, and
//   - the newest notice about the connection must not be a disconnect: "Remote Control
//     disconnected" or "not started here" (both system/informational) against "is active"
//     (system/bridge_status).
//
// Counting entries rather than time keeps an idle but connected session lit, since an idle
// session writes nothing. What it cannot see: a disconnect that left no notice (such as
// turning it off yourself), a notice further back than rcScanCap, and a resumed session that
// inherited a bridge-session line without ever connecting.
const (
	rcMarker = `{"type":"bridge-session"`
	// Notices are short system entries (about 500 bytes). Checking only lines up to this long
	// keeps the scan of a blob-heavy tail cheap.
	rcNoticeMaxLen = 8 << 10
	// Once a marker is in range, notices are looked for this far back only. Real transcripts
	// put the newest one within 561 KiB at the 99th percentile.
	rcNoticeCap = 1 << 20
	// The tail is read in chunks of at most this size.
	rcChunkMax      = 256 << 10
	rcStatusActive  = `"subtype":"bridge_status"`
	rcSubInfo       = `"subtype":"informational"`
	rcNoticeDown    = `"content":"Remote Control disconnected`
	rcNoticeNot     = `"content":"Remote Control not started`
	rcMaxLinesAfter = 100
	// rcScanCap bounds the tail read. A marker further back than this is treated as stale.
	// The largest gap between two real bridge lines was 1.7 MB.
	rcScanCap = 4 << 20
)

// remoteControlActive reports whether the transcript at path shows Remote Control on now. It
// reads the file backwards in chunks that double, so a long transcript costs one small read.
// An unreadable or missing file is not on.
func remoteControlActive(path string) bool {
	if !isFile(path) { // os.Open on a FIFO blocks
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	end, after, marker, active := st.Size(), 0, false, false
	floor := max(0, end-rcScanCap)
	var carry []byte // the first, possibly partial, line of the chunk read last
	for chunk := int64(64 << 10); end > floor; chunk = min(chunk*2, rcChunkMax) {
		if marker && st.Size()-end >= rcNoticeCap {
			return true // no notice in range: the marker alone decides
		}
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
			ln := lines[i]
			switch {
			case bytes.HasPrefix(ln, []byte(rcMarker)):
				marker = marker || after <= rcMaxLinesAfter
				if marker && active {
					return true
				}
			case len(ln) > rcNoticeMaxLen || active: // once the newest notice is "active", older ones do not matter
			case bytes.Contains(ln, []byte(rcStatusActive)):
				if marker {
					return true // the newest notice says connected
				}
				active = true // Claude Code writes the marker just before this notice
			case bytes.Contains(ln, []byte(rcSubInfo)) &&
				(bytes.Contains(ln, []byte(rcNoticeDown)) || bytes.Contains(ln, []byte(rcNoticeNot))):
				return false
			}
			if after++; after > rcMaxLinesAfter && !marker {
				return false
			}
		}
		carry, end = lines[0], start
	}
	return marker // no notice in the scanned tail: the marker alone decides
}
