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
	"bytes"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// The planner has no tools, so the only file contents it sees are the ones in
// its prompt. Besides the files the issue names, it gets the tracked files
// that share the most words with the issue, ranked by a deterministic scorer:
// the same checkout and issue always give the same files.
//
// pkg/agent/repomap is not reused: it indexes only non-test Go files, ranks
// them by their Go declarations and walks the file system rather than the
// tracked list, while an issue here may be about documentation, configuration
// or any language.
//
// The score is BM25 (k1 1.2, b 0.75) over the words of the first
// scoreReadCap bytes of each tracked text file, plus pathTermBoost times a
// term's IDF when the term is a word of the file's path. Words are split
// at non-alphanumerics and camelCase boundaries and lowercased; -ies and -ied
// fold to -y and a plural -s is dropped. A word is kept when three or more
// characters long and not English filler. A title word counts twice. Ties
// break by path.

const (
	// defaultContextBytes is the --context-bytes default: the budget for
	// the contents of the named and the ranked files together.
	defaultContextBytes = 64 * 1024
	// defaultContextFiles is the --context-files default.
	defaultContextFiles = 6
	// relatedFileCap bounds one ranked file, so a long one cannot take the
	// whole budget; a longer file is cut there and marked.
	relatedFileCap = 16 * 1024
	// scoreReadCap is how much of each file the scorer reads.
	scoreReadCap = 64 * 1024
	// scoreSizeLimit skips larger files entirely: they are data, not prose
	// or code a planner reads.
	scoreSizeLimit = 1 << 20
	// minRelativeScore drops a file scoring under this fraction of the top
	// file: an incidental match is not related.
	minRelativeScore = 0.25
	pathTermBoost    = 2.0
	titleTermWeight  = 2.0
	bm25K1           = 1.2
	bm25B            = 0.75
)

// unrankedFiles are tracked files never worth ranking: lockfiles match every
// dependency name an issue mentions and say nothing about the change.
var unrankedFiles = map[string]bool{
	"go.sum": true, "Cargo.lock": true, "package-lock.json": true, "yarn.lock": true,
	"pnpm-lock.yaml": true, "poetry.lock": true, "uv.lock": true, "Gemfile.lock": true,
	"composer.lock": true, "flake.lock": true,
}

// unrankedDirs are directories of vendored code, never ranked.
var unrankedDirs = map[string]bool{"vendor": true, "node_modules": true}

// rankedDoc is one scored file's statistics for the query's terms.
type rankedDoc struct {
	path   string
	length int
	tf     map[string]int
	inPath map[string]bool
	score  float64
}

// relatedFileContents returns the tracked files most related to the issue's
// title and body, best first, skipping the paths in skip (the files the
// issue names). At most maxFiles are returned and their contents total at
// most budget bytes; each is capped at relatedFileCap and marked when cut.
// Binary files and anything readRegularFileInside refuses (symlinks, files
// outside root) are never ranked.
func relatedFileContents(root string, files []string, title, body string, skip map[string]bool,
	budget, maxFiles int) []namedFile {
	if budget <= 0 || maxFiles <= 0 {
		return nil
	}
	query := queryTerms(title, body)
	if len(query) == 0 {
		return nil
	}
	docs := scanDocs(root, files, skip, query)
	ranked := rankDocs(docs, query)

	var out []namedFile
	for _, d := range ranked {
		if len(out) >= maxFiles || budget <= 0 {
			break
		}
		b, ok := readRegularFileInside(root, d.path)
		if !ok || bytes.IndexByte(b, 0) >= 0 {
			continue
		}
		limit := min(relatedFileCap, budget)
		nf := namedFile{Path: d.path, Content: string(b)}
		if len(b) > limit {
			nf.Content, nf.Truncated = cutUTF8(string(b), limit), true
		}
		budget -= len(nf.Content)
		out = append(out, nf)
	}
	return out
}

// scanDocs reads the head of every rankable tracked file and counts the
// query's terms in it.
func scanDocs(root string, files []string, skip map[string]bool, query map[string]float64) []rankedDoc {
	var docs []rankedDoc
	for _, f := range files {
		if skip[f] || !rankable(f) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > scoreSizeLimit {
			continue
		}
		b, ok := readRegularFileInside(root, f)
		if !ok {
			continue
		}
		if len(b) > scoreReadCap {
			b = b[:scoreReadCap]
		}
		if bytes.IndexByte(b, 0) >= 0 {
			continue
		}
		d := rankedDoc{path: f, tf: map[string]int{}, inPath: map[string]bool{}}
		eachTerm(string(b), func(t string) {
			d.length++
			if query[t] > 0 {
				d.tf[t]++
			}
		})
		eachTerm(f, func(t string) {
			if query[t] > 0 {
				d.inPath[t] = true
			}
		})
		docs = append(docs, d)
	}
	return docs
}

// rankDocs scores docs against the query and returns the related ones, best
// first.
func rankDocs(docs []rankedDoc, query map[string]float64) []rankedDoc {
	if len(docs) == 0 {
		return nil
	}
	df := map[string]int{}
	total := 0
	for _, d := range docs {
		total += d.length
		for t := range query {
			if d.tf[t] > 0 || d.inPath[t] {
				df[t]++
			}
		}
	}
	n := float64(len(docs))
	avg := math.Max(float64(total)/n, 1)
	for i := range docs {
		d := &docs[i]
		norm := bm25K1 * (1 - bm25B + bm25B*float64(d.length)/avg)
		for t, w := range query {
			if df[t] == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[t])+0.5)/(float64(df[t])+0.5))
			tf := float64(d.tf[t])
			s := idf * tf * (bm25K1 + 1) / (tf + norm)
			if d.inPath[t] {
				s += pathTermBoost * idf
			}
			d.score += w * s
		}
	}
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].score != docs[j].score {
			return docs[i].score > docs[j].score
		}
		return docs[i].path < docs[j].path
	})
	if docs[0].score <= 0 {
		return nil
	}
	cut := docs[0].score * minRelativeScore
	end := sort.Search(len(docs), func(i int) bool { return docs[i].score < cut || docs[i].score <= 0 })
	return docs[:end]
}

// rankable reports whether a tracked path may be ranked at all.
func rankable(f string) bool {
	if unrankedFiles[path.Base(f)] {
		return false
	}
	for _, seg := range strings.Split(path.Dir(f), "/") {
		if unrankedDirs[seg] {
			return false
		}
	}
	return true
}

// queryTerms weighs the issue's terms: a title term counts titleTermWeight,
// a body term 1.
func queryTerms(title, body string) map[string]float64 {
	q := map[string]float64{}
	eachTerm(body, func(t string) { q[t] = 1 })
	eachTerm(title, func(t string) { q[t] = titleTermWeight })
	return q
}

// wordRE matches the alphanumeric runs terms are cut from.
var wordRE = regexp.MustCompile(`[A-Za-z0-9]+`)

// eachTerm calls fn for each term of s, in order: every camelCase part of a
// word, and the whole word when it has more than one part, so
// "ParsePlanVerdict" gives parse, plan, verdict and parseplanverdict.
func eachTerm(s string, fn func(string)) {
	for _, w := range wordRE.FindAllString(s, -1) {
		parts := camelParts(w)
		for _, p := range parts {
			if t, ok := normTerm(p); ok {
				fn(t)
			}
		}
		if len(parts) > 1 {
			if t, ok := normTerm(w); ok {
				fn(t)
			}
		}
	}
}

// camelParts splits a word at lower-to-upper and acronym-to-word
// boundaries: "HTTPServerConfig" gives HTTP, Server, Config.
func camelParts(w string) []string {
	r := []rune(w)
	var parts []string
	start := 0
	for i := 1; i < len(r); i++ {
		lowerToUpper := unicode.IsLower(r[i-1]) && unicode.IsUpper(r[i])
		acronymEnd := i+1 < len(r) && unicode.IsUpper(r[i-1]) && unicode.IsUpper(r[i]) && unicode.IsLower(r[i+1])
		if lowerToUpper || acronymEnd {
			parts = append(parts, string(r[start:i]))
			start = i
		}
	}
	return append(parts, string(r[start:]))
}

// normTerm lowercases a word part and folds the endings that most often
// split one word in two ("retries", "retried" and "retry"; "attempts" and
// "attempt"); it refuses short parts, numbers and English filler.
func normTerm(p string) (string, bool) {
	t := strings.ToLower(p)
	switch {
	case len(t) > 4 && (strings.HasSuffix(t, "ies") || strings.HasSuffix(t, "ied")):
		t = t[:len(t)-3] + "y"
	case len(t) > 4 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss"):
		t = t[:len(t)-1]
	}
	if len(t) < 3 || stopTerms[t] || strings.Trim(t, "0123456789") == "" {
		return "", false
	}
	return t, true
}

// stopTerms are English filler words, which would reward any prose file.
var stopTerms = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		about above after again against all also and any are aren because been before being
		below between both but can cannot could did does doing done down during each
		either else even ever every few for from further get gets had has have having her
		here hers him his how however into its itself just let like may might more most
		much must need needs nor not now off once one only onto other our ours out over own
		per same she should since some such than that the their theirs them then there
		these they this those through too two under until upon use used uses using very via
		was wasn were what when where whether which while who whom whose why will with
		within without would yet you your yours`) {
		m[w] = true
	}
	return m
}()
