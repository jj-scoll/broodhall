package main

import "strings"

// The memory search analyzer. One function turns text into index terms, for documents and
// for queries alike:
//
//   - words are runs of ASCII [A-Za-z0-9._]; anything else (including non-ASCII) separates;
//   - leading and trailing '.' and '_' are trimmed, so "pass." is "pass";
//   - each word yields its lowercase whole form (which counts toward field length), and,
//     when it is a compound (camelCase, HTTPServer, a.b, a_b), its lowercase parts too;
//     parts shorter than 2 bytes or all digits are dropped, and letter/digit boundaries
//     are not split (cre8, sha256 and v2 stay whole);
//   - alphabetic terms of 4 or more letters are Porter-stemmed; terms holding digits, '.'
//     or '_' are left alone.

// memdTok is one analyzed term. whole marks a word's own form, as opposed to a part of it.
type memdTok struct {
	term  string
	whole bool
}

// memdAnalyzer holds a stem cache and scratch space; it is not safe for concurrent use.
type memdAnalyzer struct {
	stems map[string]string
	toks  []memdTok
}

func newMemdAnalyzer() *memdAnalyzer { return &memdAnalyzer{stems: make(map[string]string, 1024)} }

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_'
}
func isUpperByte(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLowerByte(c byte) bool { return c >= 'a' && c <= 'z' }

// analyze returns the terms of text in reading order. The slice is reused by the next call.
func (a *memdAnalyzer) analyze(text string) []memdTok {
	a.toks = a.toks[:0]
	for i := 0; i < len(text); {
		for i < len(text) && !isWordByte(text[i]) {
			i++
		}
		j := i
		for j < len(text) && isWordByte(text[j]) {
			j++
		}
		if j > i {
			a.word(text[i:j])
		}
		i = j
	}
	return a.toks
}

func (a *memdAnalyzer) word(w string) {
	w = strings.Trim(w, "._")
	if w == "" {
		return
	}
	a.toks = append(a.toks, memdTok{a.stem(strings.ToLower(w)), true})
	// Parts: split on '.', '_' and camel boundaries.
	start, nparts, first := 0, 0, len(a.toks)
	flush := func(end int) {
		if end <= start {
			return
		}
		p := w[start:end]
		if len(p) < 2 || allDigits(p) {
			return
		}
		a.toks = append(a.toks, memdTok{a.stem(strings.ToLower(p)), false})
		nparts++
	}
	for i := 0; i < len(w); i++ {
		c := w[i]
		if c == '.' || c == '_' {
			flush(i)
			start = i + 1
			continue
		}
		if i > start {
			p := w[i-1]
			if isLowerByte(p) && isUpperByte(c) || // fooBar
				isUpperByte(p) && isUpperByte(c) && i+1 < len(w) && isLowerByte(w[i+1]) { // HTTPServer
				flush(i)
				start = i
			}
		}
	}
	flush(len(w))
	if nparts < 2 { // a single part is the word itself (or a fragment of it): no parts
		a.toks = a.toks[:first]
	}
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (a *memdAnalyzer) stem(t string) string {
	if len(t) < 4 {
		return t
	}
	for i := 0; i < len(t); i++ {
		if t[i] < 'a' || t[i] > 'z' {
			return t
		}
	}
	if s, ok := a.stems[t]; ok {
		return s
	}
	s := memdStem(t)
	a.stems[t] = s
	return s
}

// memdStem is the Porter (1980) stemmer for lowercase ASCII words.
func memdStem(s string) string {
	if len(s) < 3 {
		return s
	}
	b := []byte(s)
	b = porter1a(b)
	b = porter1b(b)
	b = porter1c(b)
	b = porterSuffixes(b, step2Rules, 1)
	b = porterSuffixes(b, step3Rules, 1)
	b = porter4(b)
	b = porter5(b)
	return string(b)
}

func isCons(b []byte, i int) bool {
	switch b[i] {
	case 'a', 'e', 'i', 'o', 'u':
		return false
	case 'y':
		return i == 0 || !isCons(b, i-1)
	}
	return true
}

// measure is Porter's m: the number of VC sequences in b.
func measure(b []byte) int {
	n, i := 0, 0
	for i < len(b) && isCons(b, i) {
		i++
	}
	for i < len(b) {
		for i < len(b) && !isCons(b, i) {
			i++
		}
		if i >= len(b) {
			break
		}
		n++
		for i < len(b) && isCons(b, i) {
			i++
		}
	}
	return n
}

func hasVowel(b []byte) bool {
	for i := range b {
		if !isCons(b, i) {
			return true
		}
	}
	return false
}

func endsDoubleCons(b []byte) bool {
	n := len(b)
	return n >= 2 && b[n-1] == b[n-2] && isCons(b, n-1)
}

// endsCVC is Porter's *o: consonant-vowel-consonant, the last not w, x or y.
func endsCVC(b []byte) bool {
	n := len(b)
	if n < 3 || !isCons(b, n-1) || isCons(b, n-2) || !isCons(b, n-3) {
		return false
	}
	switch b[n-1] {
	case 'w', 'x', 'y':
		return false
	}
	return true
}

func hasSuffix(b []byte, suf string) bool {
	return len(b) >= len(suf) && string(b[len(b)-len(suf):]) == suf
}

func porter1a(b []byte) []byte {
	switch {
	case hasSuffix(b, "sses"):
		return b[:len(b)-2]
	case hasSuffix(b, "ies"):
		return b[:len(b)-2]
	case hasSuffix(b, "ss"):
		return b
	case hasSuffix(b, "s"):
		return b[:len(b)-1]
	}
	return b
}

func porter1b(b []byte) []byte {
	if hasSuffix(b, "eed") {
		if measure(b[:len(b)-3]) > 0 {
			return b[:len(b)-1]
		}
		return b
	}
	var stem []byte
	switch {
	case hasSuffix(b, "ed"):
		stem = b[:len(b)-2]
	case hasSuffix(b, "ing"):
		stem = b[:len(b)-3]
	default:
		return b
	}
	if !hasVowel(stem) {
		return b
	}
	b = stem
	switch {
	case hasSuffix(b, "at"), hasSuffix(b, "bl"), hasSuffix(b, "iz"):
		return append(b, 'e')
	case endsDoubleCons(b):
		switch b[len(b)-1] {
		case 'l', 's', 'z':
			return b
		}
		return b[:len(b)-1]
	case measure(b) == 1 && endsCVC(b):
		return append(b, 'e')
	}
	return b
}

func porter1c(b []byte) []byte {
	if hasSuffix(b, "y") && hasVowel(b[:len(b)-1]) {
		b[len(b)-1] = 'i'
	}
	return b
}

type porterRule struct{ suf, rep string }

var step2Rules = []porterRule{
	{"ational", "ate"}, {"tional", "tion"}, {"enci", "ence"}, {"anci", "ance"}, {"izer", "ize"},
	{"abli", "able"}, {"alli", "al"}, {"entli", "ent"}, {"eli", "e"}, {"ousli", "ous"},
	{"ization", "ize"}, {"ation", "ate"}, {"ator", "ate"}, {"alism", "al"}, {"iveness", "ive"},
	{"fulness", "ful"}, {"ousness", "ous"}, {"aliti", "al"}, {"iviti", "ive"}, {"biliti", "ble"},
}

var step3Rules = []porterRule{
	{"icate", "ic"}, {"ative", ""}, {"alize", "al"}, {"iciti", "ic"}, {"ical", "ic"}, {"ful", ""}, {"ness", ""},
}

// porterSuffixes applies the first matching rule (longest suffix wins) when the remaining
// stem has measure > minM-1, i.e. m > 0 for minM 1.
func porterSuffixes(b []byte, rules []porterRule, minM int) []byte {
	best := -1
	for i, r := range rules {
		if hasSuffix(b, r.suf) && (best < 0 || len(r.suf) > len(rules[best].suf)) {
			best = i
		}
	}
	if best < 0 {
		return b
	}
	r := rules[best]
	stem := b[:len(b)-len(r.suf)]
	if measure(stem) >= minM {
		return append(stem, r.rep...)
	}
	return b
}

var step4Suffixes = []string{"al", "ance", "ence", "er", "ic", "able", "ible", "ant", "ement", "ment", "ent",
	"ion", "ou", "ism", "ate", "iti", "ous", "ive", "ize"}

func porter4(b []byte) []byte {
	best := ""
	for _, s := range step4Suffixes {
		if hasSuffix(b, s) && len(s) > len(best) {
			best = s
		}
	}
	if best == "" {
		return b
	}
	stem := b[:len(b)-len(best)]
	if best == "ion" && (len(stem) == 0 || stem[len(stem)-1] != 's' && stem[len(stem)-1] != 't') {
		return b
	}
	if measure(stem) > 1 {
		return stem
	}
	return b
}

func porter5(b []byte) []byte {
	if hasSuffix(b, "e") {
		stem := b[:len(b)-1]
		if m := measure(stem); m > 1 || m == 1 && !endsCVC(stem) {
			b = stem
		}
	}
	if len(b) > 1 && hasSuffix(b, "ll") && measure(b) > 1 {
		b = b[:len(b)-1]
	}
	return b
}
