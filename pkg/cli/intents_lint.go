/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// The intent lint checks a planned intent against the repository before it is
// queued: no model judgement, only the tracked file list. It is tuned for
// precision. A token is checked only when it is shaped like a path from the
// repository root under a top-level directory the repository tracks, so URLs,
// import paths, flags, globs and absolute paths pass unchecked, and a false
// park costs a human more than a missed one.

// The lint check names. They reach the public park comment through the
// reason, so they are fixed strings and never model text.
const (
	lintUnknownPaths    = "unknown paths"
	lintUnmentionedFile = "files the intent never mentions"
	reasonLintPrefix    = "intent failed lint: "
	// lintLinesCap bounds the offending intent lines kept per check.
	lintLinesCap = 20
)

// repoPaths indexes a checkout's tracked files for the lint.
type repoPaths struct {
	files map[string]bool
	// dirs holds every directory a tracked file is under.
	dirs map[string]bool
	// top holds the top-level directories; only a token under one of them
	// is checked.
	top map[string]bool
}

func newRepoPaths(files []string) repoPaths {
	r := repoPaths{files: make(map[string]bool, len(files)), dirs: map[string]bool{}, top: map[string]bool{}}
	for _, f := range files {
		r.files[f] = true
		if first, _, ok := strings.Cut(f, "/"); ok {
			r.top[first] = true
		}
		for d := path.Dir(f); d != "." && !r.dirs[d]; d = path.Dir(d) {
			r.dirs[d] = true
		}
	}
	return r
}

// lintFailure is one failed check and what failed it.
type lintFailure struct {
	Check string
	// Tokens are the offending paths, in the order the intent names them.
	Tokens []string
	// Lines are the intent lines that name an offending path.
	Lines []string
}

// intentLint is the result of linting one intent.
type intentLint struct {
	// Checked counts the distinct repository paths the intent names.
	Checked  int
	Failures []lintFailure
}

func (l intentLint) ok() bool { return len(l.Failures) == 0 }

// reason is the fixed park reason: the failed checks' names.
func (l intentLint) reason() string {
	names := make([]string, 0, len(l.Failures))
	for _, f := range l.Failures {
		names = append(names, f.Check)
	}
	return reasonLintPrefix + strings.Join(names, ", ")
}

// report is the evidence a human reads: each failed check, its tokens and
// the lines that name them.
func (l intentLint) report() string {
	var b strings.Builder
	for _, f := range l.Failures {
		fmt.Fprintf(&b, "%s: %s\n", f.Check, strings.Join(f.Tokens, ", "))
		for _, line := range f.Lines {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String()
}

// lintIntent runs the checks on a planned verdict:
//
//  1. unknown paths: every repository path the intent names is tracked (a
//     file or a directory), or is in the header's files list (a file to
//     create) or under a directory one of those creates. This also catches a
//     file the intent says to create but the header leaves out.
//  2. files the intent never mentions: every header file that already exists
//     is named in the intent, by its path or its base name.
func lintIntent(v PlanVerdict, repo repoPaths) intentLint {
	header := map[string]bool{}
	headerDirs := map[string]bool{}
	for _, f := range v.Files {
		header[f] = true
		for d := path.Dir(f); d != "." && !headerDirs[d]; d = path.Dir(d) {
			headerDirs[d] = true
		}
	}
	var res intentLint
	unknown := lintFailure{Check: lintUnknownPaths}
	checked := map[string]bool{}
	missing := map[string]bool{}
	// words holds every word of the intent and its base name, for check 2.
	words := map[string]bool{}
	for _, line := range strings.Split(v.Intent, "\n") {
		hit := false
		for _, raw := range intentWordRE.FindAllString(line, -1) {
			word := trimWord(raw)
			if word == "" {
				continue
			}
			words[word], words[path.Base(word)] = true, true
			p, ok := repoPathToken(word, repo)
			if !ok {
				continue
			}
			if !checked[p] {
				checked[p] = true
				if !repo.known(p) && !header[p] && !headerDirs[p] {
					missing[p] = true
					unknown.Tokens = append(unknown.Tokens, p)
				}
			}
			hit = hit || missing[p]
		}
		if hit {
			unknown.Lines = appendLine(unknown.Lines, line)
		}
	}
	res.Checked = len(checked)
	if len(unknown.Tokens) > 0 {
		res.Failures = append(res.Failures, unknown)
	}

	unmentioned := lintFailure{Check: lintUnmentionedFile}
	for _, f := range v.Files {
		if !repo.files[f] || words[path.Base(f)] || strings.Contains(v.Intent, f) {
			continue
		}
		unmentioned.Tokens = append(unmentioned.Tokens, f)
	}
	if len(unmentioned.Tokens) > 0 {
		res.Failures = append(res.Failures, unmentioned)
	}
	return res
}

func appendLine(lines []string, line string) []string {
	if len(lines) >= lintLinesCap {
		return lines
	}
	return append(lines, strings.TrimSpace(line))
}

// known reports whether p is a tracked file or directory, or a Go symbol in
// a tracked package directory ("pkg/cli.ParsePlanVerdict").
func (r repoPaths) known(p string) bool {
	if r.files[p] || r.dirs[p] {
		return true
	}
	dir, base := path.Split(p)
	if pkg, _, ok := strings.Cut(base, "."); ok && pkg != "" {
		return r.dirs[dir+pkg]
	}
	return false
}

// intentWordRE splits intent text into words: Markdown and shell punctuation
// end a word, and so does = (a --flag=value names its value as a word).
var intentWordRE = regexp.MustCompile("[^\\s`'\"()<>,;|=]+")

// trimWord drops Markdown emphasis and sentence punctuation around a word.
func trimWord(w string) string {
	w = strings.Trim(w, "*_~")
	w = strings.TrimRight(w, ".:!?")
	return w
}

// repoPathToken returns the repository path a word names, if it is shaped
// like one: relative, under a tracked top-level directory, with no glob,
// variable or template characters. Everything from the first : or # is
// dropped (a :line, an #anchor, or a URL after its scheme), and so is a
// leading ./ or a trailing / or /... (a directory or a Go package pattern).
func repoPathToken(w string, repo repoPaths) (string, bool) {
	w, _, _ = strings.Cut(w, "#")
	w, _, _ = strings.Cut(w, ":")
	w = strings.TrimPrefix(w, "./")
	w = strings.TrimRight(w, "/.")
	if !strings.Contains(w, "/") || strings.HasPrefix(w, "/") || strings.HasPrefix(w, "-") ||
		strings.ContainsAny(w, "*?[]{}$@~%\\") {
		return "", false
	}
	first, _, _ := strings.Cut(w, "/")
	if !repo.top[first] {
		return "", false
	}
	p := path.Clean(w)
	if p == "." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}
