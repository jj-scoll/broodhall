package main

// Stale-note detection. When the observer stores an observation's files it also records a
// fingerprint of each file (observation_files.fingerprint) and the absolute path it was taken
// from (fp_path). Where notes are shown, the fingerprints are compared with the files now and
// a note whose file changed or disappeared is marked. Old rows have no fingerprint and are
// never marked.
//
// The fingerprint is the git blob hash of the file's bytes: sha1("blob <size>\0" + content),
// the value `git hash-object --no-filters <file>` prints, computed in process (no git
// process, works for tracked and untracked files alike), prefixed "blob:".

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	// staleMaxBytes: files larger than this are neither fingerprinted nor re-hashed.
	staleMaxBytes = 1 << 20
	// staleMaxHash caps how many files one run (one context build or one tool call) hashes.
	// Past it, files are left unmarked.
	staleMaxHash = 200
)

type staleState int

const (
	fileUnknown staleState = iota // no fingerprint, unreadable, too large, over the cap, or not on this machine
	fileFresh
	fileChanged
	fileGone
)

// obsFile is one observation_files row with the fingerprint it was stored with.
type obsFile struct {
	path, kind, fp, fpPath string
}

// blobHash returns "blob:<sha1>" for a regular file of at most staleMaxBytes, else "".
func blobHash(p string) string {
	// Stat before open: opening a FIFO with no writer blocks forever. O_NONBLOCK covers a
	// path swapped for a FIFO between the Stat and the open (it does nothing to regular files).
	if pi, err := os.Stat(p); err != nil || !pi.Mode().IsRegular() {
		return ""
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > staleMaxBytes {
		return ""
	}
	h := sha1.New()
	io.WriteString(h, "blob "+strconv.FormatInt(fi.Size(), 10)+"\x00")
	n, err := io.Copy(h, io.LimitReader(f, fi.Size()+1))
	if err != nil || n != fi.Size() { // read error, or the file changed size while hashing
		return ""
	}
	return "blob:" + hex.EncodeToString(h.Sum(nil))
}

// fileFingerprint is the fingerprint stored for p (an absolute path), or "" when the file is
// unreadable, not a regular file, over staleMaxBytes or a protected secret file (g may be nil).
func fileFingerprint(p string, g *secretGuard) string {
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	if g != nil {
		if file, _ := g.protected(p); file {
			return ""
		}
	}
	return blobHash(p)
}

// staleChecker compares stored fingerprints with the files now. One checker serves one run:
// it caches the result per path and hashes at most maxHash files.
type staleChecker struct {
	guard   *secretGuard
	maxHash int
	hashed  int
	cache   map[string]curFile
}

type curFile struct {
	fp string // current fingerprint
	st staleState
}

func newStaleChecker() *staleChecker {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	return &staleChecker{
		guard:   newSecretGuard(home, os.Getenv("XDG_CONFIG_HOME"), cwd),
		maxHash: staleMaxHash,
		cache:   map[string]curFile{},
	}
}

// current looks at p once per run. st is fileUnknown when it cannot say, fileGone when the file
// is missing from a directory that exists, else fileFresh with fp set.
func (c *staleChecker) current(p string) curFile {
	if cf, ok := c.cache[p]; ok {
		return cf
	}
	cf := c.look(p)
	c.cache[p] = cf
	return cf
}

func (c *staleChecker) look(p string) curFile {
	if !filepath.IsAbs(p) {
		return curFile{}
	}
	if c.guard != nil {
		if file, _ := c.guard.protected(p); file {
			return curFile{}
		}
	}
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A note from another machine names directories that are not here: only a file
			// missing from a directory that exists counts as removed.
			if di, derr := os.Stat(filepath.Dir(p)); derr == nil && di.IsDir() {
				return curFile{st: fileGone}
			}
		}
		return curFile{}
	}
	if !fi.Mode().IsRegular() || fi.Size() > staleMaxBytes || c.hashed >= c.maxHash {
		return curFile{}
	}
	c.hashed++
	fp := blobHash(p)
	if fp == "" {
		return curFile{}
	}
	return curFile{fp: fp, st: fileFresh}
}

// state compares the stored fingerprint of the file at path with the file now.
func (c *staleChecker) state(path, fp string) staleState {
	if fp == "" || path == "" {
		return fileUnknown
	}
	cur := c.current(path)
	switch {
	case cur.st == fileGone:
		return fileGone
	case cur.st == fileFresh && cur.fp != fp:
		return fileChanged
	case cur.st == fileFresh:
		return fileFresh
	}
	return fileUnknown
}

const (
	staleChangedMark = " (file changed since)"
	staleGoneMark    = " (file removed since)"
)

// markStale sets o.stale and o.staleFiles on every observation whose files changed or were
// removed since the note was written. o.files must be loaded.
func (c *staleChecker) markStale(obs []*qObs) {
	for _, o := range obs {
		o.stale, o.staleFiles = "", nil
		changed, gone := false, false
		for _, f := range o.files {
			switch c.state(f.fpPath, f.fp) {
			case fileChanged:
				changed = true
				o.staleFiles = append(o.staleFiles, f.path+" (changed)")
			case fileGone:
				gone = true
				o.staleFiles = append(o.staleFiles, f.path+" (removed)")
			}
		}
		switch {
		case changed:
			o.stale = staleChangedMark
		case gone:
			o.stale = staleGoneMark
		}
	}
}

// demoteStale moves stale notes behind fresh ones, keeping the order within each group.
func demoteStale(obs []*qObs) {
	sort.SliceStable(obs, func(i, j int) bool { return obs[i].stale == "" && obs[j].stale != "" })
}

// withStale appends the stale mark to a title.
func withStale(title string, o *qObs) string { return title + o.stale }

// recordFingerprints returns, for each path an observation names, the absolute path and the
// fingerprint to store ("" when none). A relative path is resolved against the session cwd.
func recordFingerprints(cwd string, paths []string) map[string][2]string {
	out := make(map[string][2]string, len(paths))
	guard := newStaleChecker().guard
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) {
			if cwd == "" || !filepath.IsAbs(cwd) {
				continue
			}
			abs = filepath.Join(cwd, abs)
		}
		abs = filepath.Clean(abs)
		if strings.HasPrefix(p, "~") {
			continue
		}
		if fp := fileFingerprint(abs, guard); fp != "" {
			out[p] = [2]string{fp, abs}
		}
	}
	return out
}
