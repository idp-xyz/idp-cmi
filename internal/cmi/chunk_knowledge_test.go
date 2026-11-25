package cmi

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkKnowledgePlainMatchesChunk(t *testing.T) {
	text := "alpha beta.\n\n" + strings.Repeat("delta ", 400) + "\n\nlast"
	got := ChunkKnowledge(text)
	want := Chunk(text, KnowledgeChunkBytes)
	if len(got) != len(want) {
		t.Fatalf("pieces %d, chunk %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Body != want[i] || got[i].Path() != "" || got[i].Table != "" || got[i].Page != 0 {
			t.Fatalf("piece %d %#v", i, got[i])
		}
		if got[i].IndexText("A title", false) != got[i].Body {
			t.Fatal("with the context setting off the index text is the body, whatever the title")
		}
		if got[i].IndexText("A title", true) != "A title\n"+got[i].Body {
			t.Fatalf("with the context setting on a plain piece carries the document title: %q", got[i].IndexText("A title", true))
		}
	}
}

func TestChunkKnowledgeHeadingsTablesAndPages(t *testing.T) {
	text := "# Fuel\n\nThe surcharge is waived.\n\n## Weekend\n\nSaturday is free.\n\n<!-- page: 4 -->\n<!-- table: rates -->\n" +
		"| code | rate |\n| --- | --- |\n| BAF | 12 |\n" + strings.Repeat("| x | 1 |\n", 80) +
		"\n### After\n\nDone.\n"
	text = strings.ReplaceAll(text, strings.Repeat("| x | 1 |\n", 80), strings.Repeat("| x | 1 |\n", 20)) // a table that fits the cap stays whole
	got := ChunkKnowledge(text)
	if len(got) != 4 {
		for i, c := range got {
			t.Logf("%d path=%q table=%q page=%d body=%q", i, c.Path(), c.Table, c.Page, trunc(c.Body))
		}
		t.Fatalf("pieces %d", len(got))
	}
	if got[0].Path() != "Fuel" || got[0].Body != "The surcharge is waived." || got[0].Page != 0 {
		t.Fatalf("%#v", got[0])
	}
	if got[1].Path() != "Fuel › Weekend" || !strings.Contains(got[1].Body, "Saturday") {
		t.Fatalf("%#v", got[1])
	}
	if got[2].Table != "rates" || got[2].Page != 4 || !strings.HasPrefix(got[2].Body, "| code | rate |\n| --- | --- |\n| BAF | 12 |") {
		t.Fatalf("table unmarked or its head lost: page %d table %q", got[2].Page, got[2].Table)
	}
	if got[3].Path() != "Fuel › Weekend › After" || got[3].Page != 4 || got[3].Body != "Done." {
		t.Fatalf("%#v", got[3])
	}
	index := got[2].IndexText("Price book", true)
	if !strings.HasPrefix(index, "Price book › Fuel › Weekend › rates\n") || strings.Contains(strings.SplitN(index, "\n", 2)[1], "Price book") {
		t.Fatalf("index %q", index)
	}
	if got, want := got[2].IndexText("Price book", false), "Fuel › Weekend › rates\n"+got[2].Body; got != want {
		t.Fatalf("setting off: the heading path and the body, no title: %q", got)
	}
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

func tableText(rows int, prefix string) string {
	var b strings.Builder
	b.WriteString("| code | rate |\n| --- | --- |\n")
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&b, "| %s%06d | %d |\n", prefix, i, i)
	}
	return b.String()
}

// B3: a table larger than the cap is cut into row groups, each repeating the header and the separator, each within the cap.
func TestLargeTableIsSplitIntoRowGroupsThatRepeatTheHeader(t *testing.T) {
	text := "# Rates\n\n<!-- page: 7 -->\n<!-- table: rates -->\n" + tableText(3000, "zq")
	got := ChunkKnowledge(text)
	if len(got) < 20 {
		t.Fatalf("%d pieces for a table of %d bytes", len(got), len(text))
	}
	next := 0
	for i, c := range got {
		if len(c.Body) > KnowledgeChunkBytes {
			t.Fatalf("piece %d is %d bytes", i, len(c.Body))
		}
		if !strings.HasPrefix(c.Body, "| code | rate |\n| --- | --- |\n") {
			t.Fatalf("piece %d lost the header: %q", i, c.Body[:40])
		}
		if c.Table != "rates" || c.Page != 7 || c.Path() != "Rates" {
			t.Fatalf("piece %d: table %q page %d path %q", i, c.Table, c.Page, c.Path())
		}
		for _, row := range strings.Split(strings.TrimSpace(c.Body), "\n")[2:] {
			if want := fmt.Sprintf("| zq%06d | %d |", next, next); row != want {
				t.Fatalf("piece %d: row %q, want %q (a row was lost, doubled or reordered)", i, row, want)
			}
			next++
		}
	}
	if next != 3000 {
		t.Fatalf("%d rows kept of 3000", next)
	}
	// a table that fits stays whole
	if got := ChunkKnowledge("| a | b |\n| - | - |\n| 1 | 2 |\n"); len(got) != 1 || strings.Count(got[0].Body, "\n") != 2 {
		t.Fatalf("small table: %#v", got)
	}
}

// B3: a single row longer than the cap is cut like text; nothing is lost and no character is split.
func TestRowLongerThanTheCapIsCutLikeText(t *testing.T) {
	long := strings.Repeat("运费按重量计算，每公斤十二元。", 150) // about 4 KiB without a space
	text := "| code | note |\n| --- | --- |\n| a | short |\n| b | " + long + " |\n| c | short |\n"
	got := ChunkKnowledge(text)
	var all strings.Builder
	for i, c := range got {
		if len(c.Body) > KnowledgeChunkBytes || !utf8.ValidString(c.Body) {
			t.Fatalf("piece %d: %d bytes, valid utf-8 %v", i, len(c.Body), utf8.ValidString(c.Body))
		}
		all.WriteString(c.Body)
	}
	if strings.Count(all.String(), "运费按重量计算，每公斤十二元。") != 150 || !strings.Contains(all.String(), "| c | short |") || !strings.Contains(all.String(), "| a | short |") {
		t.Fatal("text lost in the cutting of a long row")
	}
}

// S9: a fenced block is opaque to the structure parser.
func TestFencedCodeIsOpaque(t *testing.T) {
	text := "# Install\n\nRun:\n\n```sh\n# fetch the deps\nmake deps\n\n# build\n| a | b |\nmake\n<!-- page: 9 -->\n```\n\nDone.\n\n~~~\n## also not a heading\n~~~\n"
	got := ChunkKnowledge(text)
	var codes []LocatedChunk
	for _, c := range got {
		for _, h := range c.Headings {
			if h != "Install" {
				t.Fatalf("a line inside a fence became a heading: %q", h)
			}
		}
		if strings.HasPrefix(c.Body, "```") || strings.HasPrefix(c.Body, "~~~") {
			codes = append(codes, c)
		}
		if c.Page != 0 || c.Table != "" {
			t.Fatalf("a marker or a table inside a fence took effect: page %d table %q", c.Page, c.Table)
		}
	}
	if len(codes) != 2 {
		t.Fatalf("%d fenced pieces: %#v", len(codes), got)
	}
	// the block is whole - blank line, comments, pipes and all - with its fences
	if want := "```sh\n# fetch the deps\nmake deps\n\n# build\n| a | b |\nmake\n<!-- page: 9 -->\n```"; codes[0].Body != want {
		t.Fatalf("fenced block %q", codes[0].Body)
	}
	if codes[1].Body != "~~~\n## also not a heading\n~~~" || codes[1].Path() != "Install" {
		t.Fatalf("tilde block %#v", codes[1])
	}
	// a longer fence is not closed by a shorter one; an unclosed fence runs to the end
	got = ChunkKnowledge("````\n```\ninner\n```\n````\nafter\n")
	if len(got) < 1 || got[0].Body != "````\n```\ninner\n```\n````" {
		t.Fatalf("nested fence: %#v", got)
	}
	got = ChunkKnowledge("# T\n\n```\nnever closed\n# not a heading\n")
	if len(got) != 1 || !strings.Contains(got[0].Body, "# not a heading") || got[0].Path() != "T" {
		t.Fatalf("unclosed fence: %#v", got)
	}
}

// S9: a fenced block longer than the cap is cut by lines, every piece wrapped in the fence.
func TestLongFencedCodeIsCutWithTheFenceRepeated(t *testing.T) {
	var b strings.Builder
	b.WriteString("```go\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "line%04d := call(%d) // 字符\n", i, i)
	}
	b.WriteString(strings.Repeat("x", 4000) + "\n```\n")
	got := ChunkKnowledge("# Code\n\n" + b.String())
	if len(got) < 8 {
		t.Fatalf("%d pieces", len(got))
	}
	var lines []string
	for i, c := range got {
		if len(c.Body) > KnowledgeChunkBytes || !utf8.ValidString(c.Body) || !strings.HasPrefix(c.Body, "```go\n") || !strings.HasSuffix(c.Body, "\n```") {
			t.Fatalf("piece %d: %d bytes, %q...%q", i, len(c.Body), c.Body[:8], c.Body[len(c.Body)-5:])
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(c.Body, "```go\n"), "\n```")
		for _, l := range strings.Split(inner, "\n") {
			lines = append(lines, l)
		}
	}
	for i := 0; i < 400; i++ {
		if want := fmt.Sprintf("line%04d := call(%d) // 字符", i, i); lines[i] != want {
			t.Fatalf("line %d: %q", i, lines[i])
		}
	}
	if joined := strings.Join(lines[400:], ""); joined != strings.Repeat("x", 4000) {
		t.Fatalf("the long line lost %d bytes", 4000-len(joined))
	}
}

// B3: the text indexed and embedded is bounded; the context is cut, the body is not.
func TestIndexTextIsBounded(t *testing.T) {
	c := LocatedChunk{Body: strings.Repeat("字", 400), Headings: []string{strings.Repeat("标题", 3000)}, Table: "t"}
	for _, on := range []bool{false, true} {
		idx := c.IndexText(strings.Repeat("T", 500), on)
		if len(idx) > 8192 || !utf8.ValidString(idx) || !strings.HasSuffix(idx, c.Body) {
			t.Fatalf("context %v: %d bytes, valid %v, body kept %v", on, len(idx), utf8.ValidString(idx), strings.HasSuffix(idx, c.Body))
		}
	}
	// a document of nothing but one huge line: every piece is bounded
	for _, c := range ChunkKnowledge(strings.Repeat("字", 400000)) {
		if len(c.IndexText("t", true)) > 8192 || len(c.Body) > 2*KnowledgeChunkBytes {
			t.Fatalf("piece of %d bytes", len(c.Body))
		}
	}
}

// the CJK cut never splits a character, in prose, in table rows and in code
func TestCJKNeverSplitsACharacter(t *testing.T) {
	cjk := strings.Repeat("运费按重量计算，每公斤十二元。", 500)
	for name, text := range map[string]string{
		"prose": "# 费率\n\n" + cjk + "\n",
		"table": "| 项 | 说明 |\n| --- | --- |\n| 运费 | " + cjk + " |\n",
		"code":  "```\n" + cjk + "\n```\n",
	} {
		got := ChunkKnowledge(text)
		if len(got) < 2 {
			t.Fatalf("%s: %d pieces", name, len(got))
		}
		for i, c := range got {
			if !utf8.ValidString(c.Body) || strings.ContainsRune(c.Body, utf8.RuneError) {
				t.Fatalf("%s piece %d is not valid UTF-8", name, i)
			}
		}
	}
}

// A prose line that merely starts with a pipe is not a table row; a setext heading is a heading; a table marker names
// the next table of its own section only.
func TestChunkerQuirks(t *testing.T) {
	got := ChunkKnowledge("The options are\n| a | b | in the shell.\nMore prose here.\n\n# H\n\ntext\n")
	for _, c := range got {
		if c.Table != "" || strings.HasPrefix(c.Body, "| a") {
			t.Fatalf("a prose line became a table: %#v", c)
		}
	}
	if len(got) != 2 || !strings.Contains(got[0].Body, "More prose here.") {
		t.Fatalf("pieces: %#v", got)
	}
	got = ChunkKnowledge("Rates\n=====\n\nThe surcharge.\n\nWeekend\n-------\n\nFree.\n")
	if len(got) != 2 || got[0].Path() != "Rates" || got[0].Body != "The surcharge." || got[1].Path() != "Rates › Weekend" || got[1].Body != "Free." {
		t.Fatalf("setext: %#v", got)
	}
	got = ChunkKnowledge("<!-- table: rates -->\n# A\n\ntext\n\n# B\n\n| x | y |\n| - | - |\n| 1 | 2 |\n")
	for _, c := range got {
		if c.Table != "" {
			t.Fatalf("the marker leaked across headings: %#v", c)
		}
	}
	// a real table is still a table, and a rule under a table row is not a heading
	got = ChunkKnowledge("| x | y |\n| - | - |\n| 1 | 2 |\n")
	if len(got) != 1 || got[0].Path() != "" {
		t.Fatalf("table: %#v", got)
	}
}

func TestChunkKnowledgeLocatesSlidesAndSheets(t *testing.T) {
	text := "<!-- slide: 1 -->\n# Plan\nBudget words.\n<!-- slide: 2 -->\n# Risks\nRisk words.\n" +
		"<!-- sheet: Rates 2026 range: A1:C5 -->\n| row | A | B | C |\n| --- | --- | --- | --- |\n| 2 | a | b | c |\n| 3 | d | e | f |\n| 5 | g | h | i |\n"
	var got []LocatedChunk
	for _, c := range ChunkKnowledge(text) {
		got = append(got, c)
	}
	if len(got) != 3 {
		t.Fatalf("%d chunks: %+v", len(got), got)
	}
	if got[0].Slide != 1 || got[0].Path() != "Plan" || got[1].Slide != 2 || got[1].Path() != "Risks" {
		t.Fatalf("slides %+v %+v", got[0], got[1])
	}
	if got[2].Sheet != "Rates 2026" || got[2].Range != "A2:C5" {
		t.Fatalf("sheet %+v", got[2])
	}
	if !strings.Contains(got[2].IndexText("", false), "Rates 2026") || !strings.Contains(got[2].Scanned(), "Rates 2026") {
		t.Fatal("the sheet name is indexed and scanned")
	}
}

func TestALargeWorksheetTableIsCutIntoRowRanges(t *testing.T) {
	var b strings.Builder
	b.WriteString("<!-- sheet: Big range: A1:B400 -->\n| row | A | B |\n| --- | --- | --- |\n")
	for r := 2; r <= 400; r++ {
		fmt.Fprintf(&b, "| %d | value-%d | other-%d |\n", r, r, r)
	}
	chunks := ChunkKnowledge(b.String())
	if len(chunks) < 3 {
		t.Fatalf("%d chunks", len(chunks))
	}
	prev := 1
	for _, c := range chunks {
		m := cellRange.FindStringSubmatch(c.Range)
		if m == nil || m[1] != "A" || m[3] != "B" {
			t.Fatalf("range %q", c.Range)
		}
		first, _ := strconv.Atoi(m[2])
		last, _ := strconv.Atoi(m[4])
		if first != prev+1 || last < first {
			t.Fatalf("range %s after row %d", c.Range, prev)
		}
		prev = last
	}
	if prev != 400 {
		t.Fatalf("the ranges end at row %d", prev)
	}
}

func TestASheetNameMayContainRangeAndArrows(t *testing.T) {
	c := ChunkKnowledge("<!-- sheet: Q1 range: totals --&gt; x range: A1:B2 -->\n| row | A |\n| --- | --- |\n| 2 | v |\n")
	if len(c) != 1 || c[0].Sheet != "Q1 range: totals --> x" || c[0].Range != "A2:B2" {
		t.Fatalf("%+v", c)
	}
}
