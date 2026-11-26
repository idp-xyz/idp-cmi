package context

import (
	"unicode"
	"unicode/utf8"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/cmi"
)

// ---- find: matching (ADR-0001 D73) ----

// folded is a text made comparable: a run of white space is one space, A-Z are a-z (ASCII only: other scripts are matched
// as they are, so a Chinese text is matched character by character), and for each character where it began and ended
// in the original, counted in characters (code points).
type folded struct {
	runes      []rune
	start, end []int
}

func foldText(s string) folded {
	f := foldFull(s)
	// the needle is the text without the white space at its ends
	for len(f.runes) > 0 && f.runes[0] == ' ' {
		f.runes, f.start, f.end = f.runes[1:], f.start[1:], f.end[1:]
	}
	for len(f.runes) > 0 && f.runes[len(f.runes)-1] == ' ' {
		n := len(f.runes) - 1
		f.runes, f.start, f.end = f.runes[:n], f.start[:n], f.end[:n]
	}
	return f
}

// foldFull is foldText without trimming (the text searched keeps its ends).
func foldFull(s string) folded {
	var f folded
	src := []rune(s)
	f.runes, f.start, f.end = make([]rune, 0, len(src)), make([]int, 0, len(src)), make([]int, 0, len(src))
	for i := 0; i < len(src); {
		r := src[i]
		if unicode.IsSpace(r) {
			j := i
			for j < len(src) && unicode.IsSpace(src[j]) {
				j++
			}
			f.runes, f.start, f.end = append(f.runes, ' '), append(f.start, i), append(f.end, j)
			i = j
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		f.runes, f.start, f.end = append(f.runes, r), append(f.start, i), append(f.end, i+1)
		i++
	}
	return f
}

// failure is the Knuth-Morris-Pratt table of a pattern: the work of a search is linear in the text, whatever the text is.
func failure(p []rune) []int {
	f := make([]int, len(p))
	for i, k := 1, 0; i < len(p); i++ {
		for k > 0 && p[i] != p[k] {
			k = f[k-1]
		}
		if p[i] == p[k] {
			k++
		}
		f[i] = k
	}
	return f
}

// kmpAll returns where the pattern is in the text as [start, end) pairs, in order, without overlaps.
func kmpAll(text, pat []rune, fail []int) [][2]int {
	var out [][2]int
	if len(pat) == 0 {
		return nil
	}
	for i, j := 0, 0; i < len(text); i++ {
		for j > 0 && text[i] != pat[j] {
			j = fail[j-1]
		}
		if text[i] == pat[j] {
			j++
		}
		if j == len(pat) {
			out = append(out, [2]int{i - len(pat) + 1, i + 1})
			j = 0
		}
	}
	return out
}

// findAll returns where the needle is in text, without overlaps, in order. Offsets and lengths are in characters of text.
func findAll(text string, needle folded) []cmi.TextHit {
	hay := foldFull(text)
	var out []cmi.TextHit
	for _, m := range kmpAll(hay.runes, needle.runes, failure(needle.runes)) {
		start, end := hay.start[m[0]], hay.end[m[1]-1]
		out = append(out, cmi.TextHit{Offset: start, Length: end - start})
	}
	return out
}

// scanned is a chunk already searched, kept for the seam with the next one.
type scanned struct {
	f       folded
	origLen int // characters of the original text
	lastEnd int // where in f the last hit ended
}

// sameSection says two consecutive chunks are cuts of one run of text, so that a text may run across the seam between them:
// the same place in the document, and not rows of a table (which repeat their header) or a worksheet.
func sameSection(a, b *cmi.Locator) bool {
	if a == nil {
		a = &cmi.Locator{}
	}
	if b == nil {
		b = &cmi.Locator{}
	}
	if a.Sheet != "" || b.Sheet != "" || a.Table != "" || b.Table != "" || a.Page != b.Page || a.Slide != b.Slide || len(a.Headings) != len(b.Headings) {
		return false
	}
	for i := range a.Headings {
		if a.Headings[i] != b.Headings[i] {
			return false
		}
	}
	return true
}

// seamHit looks for a text that begins at the end of a chunk and goes on at the start of the next (D73): the chunker cuts a
// long paragraph where the size is reached, in the middle of a word or a Chinese phrase, or between paragraphs. The end of
// the first chunk and the start of the second are joined with nothing and with one space (the white space at a cut is gone)
// and searched. It returns the hit in the first chunk, its length clipped to that chunk's end, and how many characters of
// the second chunk the text used.
func seamHit(prev *scanned, next folded, needle folded, fail []int) (cmi.TextHit, int, bool) {
	n := len(needle.runes)
	tailFrom := max(0, len(prev.f.runes)-(n-1))
	tail := prev.f.runes[tailFrom:]
	head := next.runes[:min(n-1, len(next.runes))]
	best, bestUsed, found := cmi.TextHit{}, 0, false
	bestAt := 0
	for _, join := range [][]rune{nil, {' '}} {
		joined := append(append(append([]rune{}, tail...), join...), head...)
		for _, m := range kmpAll(joined, needle.runes, fail) {
			used := m[1] - len(tail) - len(join)
			if m[0] >= len(tail) || used < 1 || tailFrom+m[0] < prev.lastEnd {
				continue
			}
			if found && m[0] >= bestAt {
				continue
			}
			off := prev.f.start[tailFrom+m[0]]
			best, bestUsed, bestAt, found = cmi.TextHit{Offset: off, Length: prev.origLen - off}, used, m[0], true
		}
	}
	return best, bestUsed, found
}

// findInChunks searches the chunks in document order, in time linear in their text, and returns for each chunk (by its place in
// the list) where the needle is in it. A text that crosses the seam between two chunks of one section is reported on the
// chunk where it starts, its length clipped to that chunk's end, and counted once: the next chunk does not report the
// characters it used (D73).
func findInChunks(chunks []postgres.DocChunk, needle folded) map[int][]cmi.TextHit {
	out := map[int][]cmi.TextHit{}
	if len(needle.runes) == 0 {
		return out
	}
	fail := failure(needle.runes)
	var prev *scanned
	for i, c := range chunks {
		f := foldFull(c.Content)
		skip := 0 // the characters of this chunk that a text begun in the previous one already used
		if prev != nil && sameSection(chunks[i-1].Locator, c.Locator) {
			if h, used, ok := seamHit(prev, f, needle, fail); ok {
				out[i-1] = append(out[i-1], h)
				skip = used
			}
		}
		cur := &scanned{f: f, origLen: utf8.RuneCountInString(c.Content)}
		for _, m := range kmpAll(f.runes, needle.runes, fail) {
			if m[0] < skip {
				continue
			}
			out[i] = append(out[i], cmi.TextHit{Offset: f.start[m[0]], Length: f.end[m[1]-1] - f.start[m[0]]})
			cur.lastEnd = m[1]
		}
		prev = cur
	}
	return out
}

// ---- find: the excerpt ----

// excerptWindow is the part of a chunk's text a find shows: from and to are the characters of the chunk it starts and ends at,
// prefix how many characters in front of it are not the chunk's (the ellipsis of a cut start).
type excerptWindow struct {
	text     string
	from, to int
	prefix   int
}

// windowAround is at most max bytes of text around the first hit, cut between characters: it starts from the hit (the
// beginning of it when the hit alone is longer) and grows to both sides, and a cut is marked with an ellipsis. A text that
// fits is given whole. White space at its ends is dropped.
func windowAround(text string, hit cmi.TextHit, max int) excerptWindow {
	src := []rune(text)
	a, b := 0, len(src)
	if len(text) > max {
		a = min(max0(hit.Offset), len(src))
		b = min(a+hit.Length, len(src))
		bytes, end := 0, a
		for end < b && bytes+utf8.RuneLen(src[end]) <= max {
			bytes += utf8.RuneLen(src[end])
			end++
		}
		b = end
		for grew := true; grew; {
			grew = false
			if a > 0 && bytes+utf8.RuneLen(src[a-1]) <= max {
				a--
				bytes += utf8.RuneLen(src[a])
				grew = true
			}
			if b < len(src) && bytes+utf8.RuneLen(src[b]) <= max {
				bytes += utf8.RuneLen(src[b])
				b++
				grew = true
			}
		}
	}
	cutStart, cutEnd := a > 0, b < len(src)
	for a < b && unicode.IsSpace(src[a]) {
		a++
	}
	for b > a && unicode.IsSpace(src[b-1]) {
		b--
	}
	w := excerptWindow{text: string(src[a:b]), from: a, to: b}
	if cutStart {
		w.text, w.prefix = "…"+w.text, 1
	}
	if cutEnd {
		w.text += "…"
	}
	return w
}

// place gives the hits that lie wholly in the window as offsets into the excerpt as it is returned: after the section line of
// secRunes characters, after the ellipsis of a cut start (D75). A hit the window cuts is not listed.
func (w excerptWindow) place(hits []cmi.TextHit, secRunes int) []cmi.TextHit {
	var out []cmi.TextHit
	for _, h := range hits {
		if h.Offset >= w.from && h.Offset+h.Length <= w.to && w.text != "" {
			out = append(out, cmi.TextHit{Offset: secRunes + w.prefix + h.Offset - w.from, Length: h.Length})
		}
	}
	return out
}

func max0(n int) int { return max(n, 0) }
