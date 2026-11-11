// Secret detection by the rules of gitleaks (https://github.com/gitleaks/gitleaks, v8.30.1),
// used as a library, next to the hand-written secretPatterns. Its rules are compiled into
// the binary, so its licence notice is kept here:
//
// MIT License
//
// Copyright (c) 2019 Zachary Rice
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package main

import (
	"strings"
	"sync"

	"github.com/zricethezav/gitleaks/v8/detect"
)

// gitleaksSkip are the default rules left out. generic-api-key matches key = value pairs
// by name and entropy, which is the key-name masking's job (it tags those [secret:tag]) and
// the main source of false positives on command lines.
var gitleaksSkip = []string{"generic-api-key"}

// gitleaksDetector builds the detector on first use: parsing the embedded rules and
// compiling their regexes costs about 20 ms, which a process that never scans must not pay.
var gitleaksDetector = sync.OnceValue(func() *detect.Detector {
	d, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		return nil
	}
	for _, id := range gitleaksSkip {
		delete(d.Config.Rules, id)
	}
	d.MaxDecodeDepth = 0         // match the text as written; a decoded hit has no span in it
	d.IgnoreGitleaksAllow = true // a "gitleaks:allow" comment must not hide a secret from the guard
	return d
})

// gitleaksSpans is every occurrence of every value the gitleaks rules report in s, named by
// rule ID (a slug already). Scans are serialised: one detector is shared, and its
// concurrent use is not something this code has verified.
func gitleaksSpans(s string) []secretSpan {
	d := gitleaksDetector()
	if d == nil {
		return nil
	}
	gitleaksMu.Lock()
	findings := d.DetectString(s)
	gitleaksMu.Unlock()
	var out []secretSpan
	seen := map[string]bool{}
	for _, f := range findings {
		if f.Secret == "" || seen[f.RuleID+"\x00"+f.Secret] {
			continue
		}
		seen[f.RuleID+"\x00"+f.Secret] = true
		for at := 0; ; {
			i := strings.Index(s[at:], f.Secret)
			if i < 0 {
				break
			}
			out = append(out, secretSpan{f.RuleID, at + i, at + i + len(f.Secret)})
			at += i + len(f.Secret)
		}
	}
	return out
}

var gitleaksMu sync.Mutex

// withoutOverlap is the spans of extra that touch none of base, so where a hand-written
// pattern and a gitleaks rule find the same value the pattern keeps its name and mask.
func withoutOverlap(extra, base []secretSpan) []secretSpan {
	var out []secretSpan
next:
	for _, e := range extra {
		for _, b := range base {
			if e.Start < b.End && b.Start < e.End {
				continue next
			}
		}
		out = append(out, e)
	}
	return out
}
