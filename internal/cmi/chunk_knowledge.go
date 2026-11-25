package cmi

import (
	"regexp"
	"strconv"
	"strings"
)

// KnowledgeChunkBytes is the byte cap of one chunk: how long a prose piece, a group of table rows or a
// code block may grow before it is split. Headings start a new piece. The number stays at the
// historical index size until a labelled set says otherwise.
const KnowledgeChunkBytes = 1500

// MaxIndexTextBytes bounds the text that is indexed (tsvector) and embedded for one chunk, whatever the
// document title and the heading path add to the body: the title and the path are cut, never the body.
// A tsvector is limited to 1 MiB and an embedder to its context; a chunk body is at most about twice
// KnowledgeChunkBytes, so this bound is never reached by a body.
const MaxIndexTextBytes = 8192

const headingJoin = " › "

var (
	pageMarker  = regexp.MustCompile(`(?i)^<!--\s*page:\s*(\d+)\s*-->$`)
	slideMarker = regexp.MustCompile(`(?i)^<!--\s*slide:\s*(\d+)\s*-->$`)
	tableMarker = regexp.MustCompile(`(?i)^<!--\s*table:\s*(.*?)\s*-->$`)
	// sheetMarker is `<!-- sheet: NAME range: A1:F100 -->`: the name is what lies between "sheet: " and the last " range: "
	// (the converter writes `-->` in a name as `--&gt;`).
	sheetMarker = regexp.MustCompile(`(?i)^<!--\s*sheet:\s*(.*)\s+range:\s*([A-Za-z]{1,3}\d+:[A-Za-z]{1,3}\d+)\s*-->$`)
	// cellRange is a range of A1 references.
	cellRange = regexp.MustCompile(`^([A-Z]{1,3})(\d+):([A-Z]{1,3})(\d+)$`)
)

// LocatedChunk is one index unit of a knowledge document. Body is what a model
// is shown. IndexText can add the document title and the heading path so a clause
// can be found without the heading words being treated as part of the excerpt.
type LocatedChunk struct {
	Body     string
	Headings []string
	Page     int    // 0 when the text has no page marker
	Slide    int    // 0 when the text has no slide marker (a presentation)
	Sheet    string // the worksheet of a spreadsheet's text ("" for other text)
	Range    string // the cells a piece of a worksheet's table covers, e.g. A12:F40
	Table    string // set when this piece is rows of a named table
}

// SplitHeadingPath is the inverse of Path.
func SplitHeadingPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, headingJoin)
}

// Scanned is everything of the chunk a model may read or a search may match: the heading path (the first line of its excerpt),
// the table name and the body. The write-time injection scan (D12) runs on this, not on the body alone.
func (c LocatedChunk) Scanned() string {
	head := strings.TrimSpace(c.Path() + "\n" + c.Sheet + "\n" + c.Table)
	if head == "" {
		return c.Body
	}
	return head + "\n" + c.Body
}

// Path is the heading path, joined for storage.
func (c LocatedChunk) Path() string {
	return strings.Join(c.Headings, headingJoin)
}

// IndexText is the string that is indexed and embedded: the heading path and the table name of the chunk, a newline, then
// the body - always, so a section can be found by the words of its heading (the headings are not in the body). With
// withTitle the document title goes in front of them (the setting `knowledge.indexContext`, off by default: that is what
// D56 measured and did not adopt). A chunk with no heading and no table name is its body alone. The result never exceeds
// MaxIndexTextBytes: the context is cut to fit, the body is not.
func (c LocatedChunk) IndexText(title string, withTitle bool) string {
	var parts []string
	if t := strings.TrimSpace(title); withTitle && t != "" {
		parts = append(parts, t)
	}
	parts = append(parts, c.Headings...)
	if c.Sheet != "" {
		parts = append(parts, c.Sheet)
	}
	if c.Table != "" {
		parts = append(parts, c.Table)
	}
	head := strings.Join(parts, headingJoin)
	body := cutUTF8(c.Body, MaxIndexTextBytes)
	if head == "" {
		return body
	}
	room := MaxIndexTextBytes - len(body) - 1
	if room <= 0 {
		return body
	}
	return cutUTF8(head, room) + "\n" + body
}

// cutUTF8 returns s cut to at most n bytes on a character boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Boundary(s, cut) {
		cut--
	}
	return s[:cut]
}

// ChunkKnowledge splits markdown-ish text on headings and splits the prose of a long section with Chunk.
// A table that does not fit KnowledgeChunkBytes is cut into groups of rows, each repeating the header
// (and separator) row; one row longer than the cap is cut like text. A fenced code block (``` or ~~~) is
// opaque - its lines are neither headings, tables nor markers - and is one chunk unless it is longer than
// the cap, when it is cut by lines with the fence repeated on every piece. A line `<!-- page: N -->`
// sets the page of the following pieces; `<!-- table: name -->` names the next table.
// Text with none of these is split exactly as Chunk does.
func ChunkKnowledge(text string) []LocatedChunk {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !hasKnowledgeStructure(text) {
		return locatePlain(text, nil, 0)
	}
	lines := strings.Split(text, "\n")
	var out []LocatedChunk
	var headings []heading
	var prose []string
	var page, slide int
	var sheet, sheetCols string
	var pendingTable string
	place := func(c LocatedChunk) LocatedChunk {
		c.Slide, c.Sheet = slide, sheet
		return c
	}
	flush := func() {
		body := strings.TrimSpace(strings.Join(prose, "\n"))
		prose = nil
		for _, c := range locatePlain(body, headingTexts(headings), page) {
			out = append(out, place(c))
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if ch, n, ok := fenceOpen(line); ok {
			flush()
			j := i + 1
			for j < len(lines) && !fenceCloses(lines[j], ch, n) {
				j++
			}
			closed := j < len(lines)
			var inner []string
			if j > i+1 {
				inner = lines[i+1 : j]
			}
			for _, body := range fenceChunks(strings.TrimSpace(line), inner, ch, n, closed, KnowledgeChunkBytes) {
				out = append(out, place(LocatedChunk{Body: body, Headings: headingTexts(headings), Page: page}))
			}
			i = j // the closing fence, or past the end
			continue
		}
		if m := pageMarker.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			flush()
			page, _ = strconv.Atoi(m[1])
			continue
		}
		if m := slideMarker.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			flush()
			slide, _ = strconv.Atoi(m[1])
			continue
		}
		if m := sheetMarker.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			flush()
			sheet, sheetCols = strings.ReplaceAll(strings.TrimSpace(m[1]), "--&gt;", "-->"), strings.ToUpper(m[2])
			pendingTable = ""
			continue
		}
		if m := tableMarker.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			flush()
			pendingTable = strings.TrimSpace(m[1])
			continue
		}
		if level, title, ok := parseHeading(line); ok {
			flush()
			headings = pushHeading(headings, level, title)
			pendingTable = "" // a table marker names the next table of its section only
			continue
		}
		if i+1 < len(lines) && setextUnderline(lines[i+1]) != 0 && strings.TrimSpace(line) != "" && !isTableLine(line) &&
			(len(prose) == 0 || strings.TrimSpace(prose[len(prose)-1]) == "") {
			flush()
			headings = pushHeading(headings, setextUnderline(lines[i+1]), strings.TrimSpace(line))
			pendingTable = ""
			i++ // the underline
			continue
		}
		if isTableLine(line) {
			flush()
			var rows []string
			for i < len(lines) && isTableLine(lines[i]) {
				rows = append(rows, lines[i])
				i++
			}
			i--
			for _, body := range tableChunks(rows, KnowledgeChunkBytes) {
				c := place(LocatedChunk{Body: body, Headings: headingTexts(headings), Page: page, Table: pendingTable})
				if sheet != "" {
					c.Range = sheetRange(body, sheetCols)
				}
				out = append(out, c)
			}
			pendingTable = ""
			continue
		}
		prose = append(prose, line)
	}
	flush()
	if len(out) == 0 {
		return locatePlain(text, nil, page)
	}
	return out
}

// sheetRange is the cells a piece of a worksheet's table covers. The converter writes the Excel row number in the first
// column of every row (the header's is "row"), so the piece's first and last row numbers and the columns of the sheet's
// marker (A1:F100 -> A and F) give A12:F40. A piece that does not start with a numbered row (the rest of a row that was cut
// like text) gets the whole range of the marker's columns' sheet block.
func sheetRange(body, whole string) string {
	m := cellRange.FindStringSubmatch(whole)
	if m == nil {
		return whole
	}
	first, last := 0, 0
	for _, line := range strings.Split(body, "\n") {
		n := rowNumber(line)
		if n == 0 {
			continue
		}
		if first == 0 {
			first = n
		}
		last = n
	}
	if first == 0 {
		return whole
	}
	return m[1] + strconv.Itoa(first) + ":" + m[3] + strconv.Itoa(last)
}

// rowNumber is the number in the first cell of a table line, or 0 (the header, the separator, text).
func rowNumber(line string) int {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "|") {
		return 0
	}
	s = strings.TrimPrefix(s, "|")
	end := strings.Index(s, "|")
	if end < 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(s[:end]))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

func locatePlain(text string, headings []string, page int) []LocatedChunk {
	var out []LocatedChunk
	for _, body := range Chunk(text, KnowledgeChunkBytes) {
		out = append(out, LocatedChunk{Body: body, Headings: append([]string(nil), headings...), Page: page})
	}
	return out
}

// cutPieces cuts s into pieces of at most n bytes on character boundaries.
func cutPieces(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := n
		for cut > 0 && !utf8Boundary(s, cut) {
			cut--
		}
		if cut == 0 {
			cut = n // n smaller than one character: cannot happen for n >= 4
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

var separatorRow = regexp.MustCompile(`^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?\s*$`)

// tableChunks returns the table's text as bodies of at most max bytes. A table that fits is one body.
// Otherwise its rows are grouped, every group starting with the header row and the separator row (when
// the table has them and they fit with a row); a row that does not fit a group alone is cut like text.
func tableChunks(rows []string, max int) []string {
	whole := strings.TrimSpace(strings.Join(rows, "\n"))
	if whole == "" {
		return nil
	}
	if len(whole) <= max {
		return []string{whole}
	}
	var head string
	body := rows
	if len(rows) >= 2 && separatorRow.MatchString(strings.TrimSpace(rows[1])) && strings.Contains(rows[1], "-") {
		head = strings.TrimRight(rows[0], " \t") + "\n" + strings.TrimRight(rows[1], " \t")
		body = rows[2:]
	}
	if len(head) > max/2 { // a header that leaves little room for rows is not repeated
		head, body = "", rows
	}
	var out []string
	cur := head
	rowsIn := 0
	emit := func() {
		if rowsIn > 0 {
			out = append(out, cur)
		}
		cur, rowsIn = head, 0
	}
	for _, row := range body {
		row = strings.TrimRight(row, " \t")
		if strings.TrimSpace(row) == "" {
			continue
		}
		if len(head)+1+len(row) > max { // the row alone does not fit under the header: cut it like text
			emit()
			out = append(out, cutPieces(row, max)...)
			continue
		}
		add := len(row)
		if cur != "" {
			add++
		}
		if len(cur)+add > max {
			emit()
		}
		if cur != "" {
			cur += "\n"
		}
		cur += row
		rowsIn++
	}
	emit()
	return out
}

// fenceOpen recognizes the opening line of a fenced code block: up to three spaces, then three or more
// backticks or tildes (a backtick fence's info string may not contain a backtick).
func fenceOpen(line string) (ch byte, n int, ok bool) {
	i := 0
	for i < len(line) && i < 4 && line[i] == ' ' {
		i++
	}
	if i > 3 || i >= len(line) || (line[i] != '`' && line[i] != '~') {
		return 0, 0, false
	}
	ch = line[i]
	j := i
	for j < len(line) && line[j] == ch {
		j++
	}
	if j-i < 3 || (ch == '`' && strings.ContainsRune(line[j:], '`')) {
		return 0, 0, false
	}
	return ch, j - i, true
}

// fenceCloses reports whether line closes a fence opened with n of ch: the same character, at least n,
// nothing but spaces after.
func fenceCloses(line string, ch byte, n int) bool {
	i := 0
	for i < len(line) && i < 4 && line[i] == ' ' {
		i++
	}
	if i > 3 {
		return false
	}
	j := i
	for j < len(line) && line[j] == ch {
		j++
	}
	return j-i >= n && strings.TrimSpace(line[j:]) == ""
}

// fenceChunks returns a fenced block as bodies of at most max bytes. A block that fits is one body, as
// written. A longer one is cut by lines (a line longer than a piece is cut like text) and every piece is
// wrapped in the opening fence line and a closing fence.
func fenceChunks(open string, inner []string, ch byte, n int, closed bool, max int) []string {
	closer := strings.Repeat(string(ch), n)
	whole := open + "\n" + strings.Join(inner, "\n")
	if closed {
		whole += "\n" + closer
	}
	whole = strings.TrimRight(whole, "\n")
	if len(whole) <= max {
		return []string{whole}
	}
	if len(open) > max/4 { // a very long info string: keep the marker only
		open = closer
	}
	room := max - len(open) - len(closer) - 2
	var out []string
	var cur []string
	size := 0
	emit := func() {
		if len(cur) > 0 {
			out = append(out, open+"\n"+strings.Join(cur, "\n")+"\n"+closer)
		}
		cur, size = nil, 0
	}
	for _, l := range inner {
		for _, piece := range cutPieces(l, room) {
			if size > 0 && size+1+len(piece) > room {
				emit()
			}
			if size > 0 {
				size++
			}
			cur = append(cur, piece)
			size += len(piece)
		}
		if l == "" { // a blank line inside the block
			if size > 0 && size+1 > room {
				emit()
			}
			if size > 0 {
				cur = append(cur, "")
				size++
			}
		}
	}
	emit()
	return out
}

func hasKnowledgeStructure(text string) bool {
	all := strings.Split(text, "\n")
	for i, line := range all {
		s := strings.TrimSpace(line)
		if _, _, ok := fenceOpen(line); ok {
			return true
		}
		if i+1 < len(all) && s != "" && setextUnderline(all[i+1]) != 0 && !isTableLine(s) && (i == 0 || strings.TrimSpace(all[i-1]) == "") {
			return true
		}
		if _, _, ok := parseHeading(s); ok || isTableLine(s) || pageMarker.MatchString(s) || slideMarker.MatchString(s) || sheetMarker.MatchString(s) || tableMarker.MatchString(s) {
			return true
		}
	}
	return false
}

type heading struct {
	level int
	text  string
}

func pushHeading(stack []heading, level int, text string) []heading {
	for len(stack) > 0 && stack[len(stack)-1].level >= level {
		stack = stack[:len(stack)-1]
	}
	return append(stack, heading{level, text})
}

func headingTexts(stack []heading) []string {
	if len(stack) == 0 {
		return nil
	}
	out := make([]string, len(stack))
	for i, h := range stack {
		out[i] = h.text
	}
	return out
}

func parseHeading(line string) (int, string, bool) {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
		return 0, "", false
	}
	title := strings.TrimSpace(line[i:])
	if title == "" {
		return 0, "", false
	}
	return i, title, true
}

// isTableLine is a pipe table row: it starts and ends with a pipe (a prose line that merely starts with one, such as a
// shell snippet, is not a row).
func isTableLine(line string) bool {
	s := strings.TrimSpace(line)
	return strings.HasPrefix(s, "|") && strings.HasSuffix(s, "|") && strings.Count(s, "|") >= 2
}

var setextRE = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)

// setextUnderline is 1 for a line of = and 2 for a line of - (the underline of a setext heading), else 0.
func setextUnderline(line string) int {
	m := setextRE.FindStringSubmatch(line)
	switch {
	case m == nil:
		return 0
	case m[1][0] == '=':
		return 1
	}
	return 2
}
