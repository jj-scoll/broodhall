// Secret detection by the rules of gitleaks (https://github.com/gitleaks/gitleaks, v8.30.1),
// next to the hand-written secretPatterns. Its rules are compiled into the binary as data
// (secretrules/rules_gen.go, which keeps the notice too), so the licence notice is kept here:
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

	"github.com/jj-scoll/broodhall/cmd/guardrails/secretrules"
)

// gitleaksSpans is every occurrence of every value the gitleaks rules report in s, named by
// rule ID (a slug already). secretrules evaluates them with no decoding and no
// "gitleaks:allow" escape, and without generic-api-key (it matches key = value pairs by name
// and entropy, which is the key-name masking's job, and is the main source of false
// positives on command lines).
func gitleaksSpans(s string) []secretSpan {
	var out []secretSpan
	seen := map[string]bool{}
	for _, f := range secretrules.Scan(s) {
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
