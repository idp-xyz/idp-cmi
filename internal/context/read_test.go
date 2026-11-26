package context

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/cmi"
)

func TestLocatorMatching(t *testing.T) {
	chunk := func(l cmi.Locator) *cmi.Locator { return &l }
	cases := []struct {
		name string
		want cmi.Locator
		have *cmi.Locator
		ok   bool
	}{
		{"page is exact", cmi.Locator{Page: 3}, chunk(cmi.Locator{Page: 3}), true},
		{"another page", cmi.Locator{Page: 3}, chunk(cmi.Locator{Page: 4}), false},
		{"a chunk with no page", cmi.Locator{Page: 3}, nil, false},
		{"slide is exact", cmi.Locator{Slide: 7}, chunk(cmi.Locator{Slide: 7}), true},
		{"another slide", cmi.Locator{Slide: 7}, chunk(cmi.Locator{Slide: 8}), false},
		{"headings are a prefix", cmi.Locator{Headings: []string{"Rates"}}, chunk(cmi.Locator{Headings: []string{"Rates", "Morning"}}), true},
		{"the whole path", cmi.Locator{Headings: []string{"Rates", "Morning"}}, chunk(cmi.Locator{Headings: []string{"Rates", "Morning"}}), true},
		{"a path longer than the chunk's", cmi.Locator{Headings: []string{"Rates", "Morning"}}, chunk(cmi.Locator{Headings: []string{"Rates"}}), false},
		{"a prefix is by heading, not by letters", cmi.Locator{Headings: []string{"Rate"}}, chunk(cmi.Locator{Headings: []string{"Rates"}}), false},
		{"the second heading is not a prefix", cmi.Locator{Headings: []string{"Morning"}}, chunk(cmi.Locator{Headings: []string{"Rates", "Morning"}}), false},
		{"a sheet is exact", cmi.Locator{Sheet: "Q1"}, chunk(cmi.Locator{Sheet: "Q1", Range: "A2:C5"}), true},
		{"another sheet", cmi.Locator{Sheet: "Q1"}, chunk(cmi.Locator{Sheet: "Q2", Range: "A2:C5"}), false},
		{"ranges that overlap", cmi.Locator{Sheet: "Q1", Range: "B4:D9"}, chunk(cmi.Locator{Sheet: "Q1", Range: "A2:C5"}), true},
		{"ranges that touch at a corner", cmi.Locator{Sheet: "Q1", Range: "C5:D9"}, chunk(cmi.Locator{Sheet: "Q1", Range: "A2:C5"}), true},
		{"ranges apart by a row", cmi.Locator{Sheet: "Q1", Range: "A6:C9"}, chunk(cmi.Locator{Sheet: "Q1", Range: "A2:C5"}), false},
		{"ranges apart by a column", cmi.Locator{Sheet: "Q1", Range: "D2:E5"}, chunk(cmi.Locator{Sheet: "Q1", Range: "A2:C5"}), false},
		{"a range inside", cmi.Locator{Sheet: "Q1", Range: "B3:B3"}, chunk(cmi.Locator{Sheet: "Q1", Range: "A2:C5"}), true},
		{"a range over several columns of letters", cmi.Locator{Range: "AA1:AB2"}, chunk(cmi.Locator{Sheet: "Q1", Range: "Z1:AA9"}), true},
		{"a range of the same sheet name elsewhere", cmi.Locator{Sheet: "Q1", Range: "A2:C5"}, chunk(cmi.Locator{Sheet: "Q2", Range: "A2:C5"}), false},
		{"a chunk with no range does not overlap", cmi.Locator{Range: "A1:B2"}, chunk(cmi.Locator{Sheet: "Q1"}), false},
		{"every field given must hold", cmi.Locator{Page: 2, Headings: []string{"Rates"}}, chunk(cmi.Locator{Page: 1, Headings: []string{"Rates"}}), false},
		{"a table is exact", cmi.Locator{Table: "T1"}, chunk(cmi.Locator{Table: "T1"}), true},
		{"another table", cmi.Locator{Table: "T1"}, chunk(cmi.Locator{Table: "T2"}), false},
	}
	for _, c := range cases {
		if got := matchesLocator(&c.want, c.have); got != c.ok {
			t.Errorf("%s: %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestReadRequestIsValidated(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	bad := []ReadRequest{
		{DocumentID: "nope"},
		{DocumentID: id, Around: ptr(6)},
		{DocumentID: id, Around: ptr(-1)},
		{DocumentID: id, Locator: &cmi.Locator{Range: "A1-B2"}},
		{DocumentID: id, Locator: &cmi.Locator{Page: -1}},
		{DocumentID: id, Locator: &cmi.Locator{Headings: []string{" "}}},
		{DocumentID: id, Budget: &cmi.Budget{MaxTokens: 32001}},
		{DocumentID: id, SensitivityCeiling: "secret"},
		{DocumentID: id, Purpose: strings.Repeat("x", 201)},
	}
	for i, r := range bad {
		if r.validate() == nil {
			t.Errorf("read request %d is accepted: %+v", i, r)
		}
	}
	if err := (&ReadRequest{DocumentID: id, Around: ptr(0), Locator: &cmi.Locator{Sheet: "Q1", Range: "A1:B2"}}).validate(); err != nil {
		t.Error(err)
	}
	badFind := []FindRequest{{DocumentID: id, Text: "   "}, {DocumentID: id, Text: ""}, {DocumentID: id, Text: "x", MaxHits: 101}, {DocumentID: id, Text: "x", MaxHits: -1},
		{DocumentID: id, Text: strings.Repeat("字", 1001)}, {DocumentID: "x", Text: "x"}}
	for i, r := range badFind {
		if r.validate() == nil {
			t.Errorf("find request %d is accepted", i)
		}
	}
	if err := (&FindRequest{DocumentID: id, Text: strings.Repeat("字", 1000), MaxHits: 100}).validate(); err != nil {
		t.Error(err)
	}
}

func ptr(n int) *int { return &n }

func hitsOf(text, needle string) []cmi.TextHit { return findAll(text, foldText(needle)) }

func TestFindMatchesByCharactersCaseAndWhiteSpace(t *testing.T) {
	cases := []struct {
		name, text, needle string
		want               []cmi.TextHit
	}{
		{"a word", "The Pickup slot is open", "pickup", []cmi.TextHit{{Offset: 4, Length: 6}}},
		{"ASCII case in the needle too", "the pickup slot", "PiCkUp", []cmi.TextHit{{Offset: 4, Length: 6}}},
		{"a substring of a word", "surcharges", "charge", []cmi.TextHit{{Offset: 3, Length: 6}}},
		{"white space in the text is one space", "late\n\n  pickup\tslot", "late pickup slot", []cmi.TextHit{{Offset: 0, Length: 19}}},
		{"white space in the needle is one space", "late pickup slot", "  late \n pickup   slot ", []cmi.TextHit{{Offset: 0, Length: 16}}},
		{"no space is not a space", "latepickup", "late pickup", nil},
		{"two hits do not overlap", "aaaa", "aa", []cmi.TextHit{{Offset: 0, Length: 2}, {Offset: 2, Length: 2}}},
		{"Chinese by character", "取件时间是上午七点，取件地点在仓库。", "取件", []cmi.TextHit{{Offset: 0, Length: 2}, {Offset: 10, Length: 2}}},
		{"Chinese is not a word search", "附加费用", "加费", []cmi.TextHit{{Offset: 1, Length: 2}}},
		{"offsets count characters after Chinese", "取件 pickup 取件 PICKUP", "pickup", []cmi.TextHit{{Offset: 3, Length: 6}, {Offset: 13, Length: 6}}},
		{"only ASCII letters fold", "ÀÉ àé", "àé", []cmi.TextHit{{Offset: 3, Length: 2}}},
		{"full-width letters do not fold", "ＡＢ", "ab", nil},
		{"an ideographic space is white space", "取件　时间", "取件 时间", []cmi.TextHit{{Offset: 0, Length: 5}}},
		{"not found", "nothing here", "pickup", nil},
		{"a needle longer than the text", "ab", "abc", nil},
	}
	for _, c := range cases {
		got := hitsOf(c.text, c.needle)
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %v, want %v", c.name, got, c.want)
			}
		}
		// the offsets point at the matched text in the original, counted in characters
		runes := []rune(c.text)
		for _, h := range got {
			if h.Offset < 0 || h.Offset+h.Length > len(runes) || h.Length < 1 {
				t.Errorf("%s: hit %v is outside the text", c.name, h)
			}
		}
	}
}

func TestAWindowIsCutBetweenCharactersAndCentredOnTheHit(t *testing.T) {
	text := strings.Repeat("字", 400) + "needle" + strings.Repeat("词", 400) // 2400 + 6 + 1200 bytes
	hit := hitsOf(text, "needle")[0]
	for _, max := range []int{7, 20, 100, 1500} {
		w := windowAround(text, hit, max).text
		if !utf8.ValidString(w) {
			t.Fatalf("max %d: the window is not valid UTF-8", max)
		}
		if !strings.Contains(w, "needle") && max >= 6+1 {
			t.Errorf("max %d: the hit is not in the window %q", max, w)
		}
		if body := strings.Trim(w, "…"); len(body) > max {
			t.Errorf("max %d: the window is %d bytes", max, len(body))
		}
		if !strings.HasPrefix(w, "…") || !strings.HasSuffix(w, "…") {
			t.Errorf("max %d: a cut at both ends is not marked: %q", max, w)
		}
	}
	// centred: about as much before as after
	w := windowAround(text, hit, 1500).text
	before, after := strings.Index(w, "needle"), len(w)-strings.Index(w, "needle")-6
	if before < 500 || after < 500 {
		t.Errorf("the hit is not in the middle of the window: %d bytes before, %d after", before, after)
	}
	// a hit longer than the window shows its beginning, still whole characters
	long := strings.Repeat("字", 600)
	w = windowAround(long, cmi.TextHit{Offset: 0, Length: 600}, 100).text
	if !utf8.ValidString(w) || len(strings.Trim(w, "…")) > 100 || !strings.HasSuffix(w, "…") {
		t.Errorf("a long hit: %q", w)
	}
	// a text that fits is whole
	if w := windowAround("short text", cmi.TextHit{Offset: 2, Length: 3}, 1500).text; w != "short text" {
		t.Errorf("%q", w)
	}
}

// the hits of an item are offsets into the excerpt as it is returned: after the section line and after the ellipsis of a cut
// start; a hit the window cuts is not listed (D75)
func TestHitsAreOffsetsIntoTheExcerptAsReturned(t *testing.T) {
	text := "  " + strings.Repeat("字", 400) + " needle " + strings.Repeat("词", 400) + " needle again " + strings.Repeat("句", 300)
	hits := hitsOf(text, "needle")
	if len(hits) != 2 {
		t.Fatalf("%v", hits)
	}
	section := "Rates › Morning\n"
	w := windowAround(text, hits[0], 1500)
	excerpt := []rune(section + w.text)
	placed := w.place(hits, utf8.RuneCountInString(section))
	if len(placed) == 0 || len(placed) > 2 {
		t.Fatalf("%v", placed)
	}
	for _, h := range placed {
		if got := string(excerpt[h.Offset : h.Offset+h.Length]); got != "needle" {
			t.Errorf("hit %v is %q in the excerpt", h, got)
		}
	}
	// a window that fits the whole text keeps white space out of the offsets
	short := "\n\n  one needle here  \n"
	h := hitsOf(short, "needle")
	w = windowAround(short, h[0], 1500)
	if got := w.place(h, 0); len(got) != 1 || w.text[got[0].Offset:got[0].Offset+got[0].Length] != "needle" {
		t.Errorf("%q %v", w.text, got)
	}
	// a hit that the cut window does not hold whole is not listed
	two := strings.Repeat("a", 1400) + " needle " + strings.Repeat("b", 1400)
	h = hitsOf(two, "needle")
	if got := windowAround(two, h[0], 100).place(h, 0); len(got) != 1 {
		t.Errorf("the hit at the centre: %v", got)
	}
	if got := windowAround(two, cmi.TextHit{Offset: 0, Length: 6}, 100).place(h, 0); len(got) != 0 {
		t.Errorf("a hit outside the window is listed: %v", got)
	}
}

func TestFoldTextKeepsWhereEachCharacterWas(t *testing.T) {
	f := foldFull("A \n b")
	if string(f.runes) != "a b" || f.start[1] != 1 || f.end[1] != 4 || f.start[2] != 4 {
		t.Fatalf("%+v", f)
	}
}

func chunksOf(loc *cmi.Locator, texts ...string) []postgres.DocChunk {
	var out []postgres.DocChunk
	for i, t := range texts {
		out = append(out, postgres.DocChunk{Seq: i, Content: t, Locator: loc})
	}
	return out
}

// hardCut marks every chunk but the last as cut where the size limit was reached (D77).
func hardCut(chunks []postgres.DocChunk) []postgres.DocChunk {
	for i := 0; i+1 < len(chunks); i++ {
		chunks[i].CutAfter = true
	}
	return chunks
}

// a text that runs across the cut between two chunks of one section is found, on the chunk where it begins, once (D73)
func TestFindCrossesTheSeamBetweenChunks(t *testing.T) {
	cut := strings.Repeat("字", 100) + "取件时"
	chunks := hardCut(chunksOf(&cmi.Locator{Headings: []string{"Rates"}}, cut, "间是上午七点。"+strings.Repeat("词", 50), "取件时间again"))
	chunks[1].CutAfter = false
	got := findInChunks(chunks, foldText("取件时间"))
	if len(got[0]) != 1 || got[0][0] != (cmi.TextHit{Offset: 100, Length: 3}) {
		t.Fatalf("on the chunk where it begins, clipped to its end: %v", got)
	}
	if len(got[1]) != 0 {
		t.Fatalf("counted again on the next chunk: %v", got[1])
	}
	if len(got[2]) != 1 || got[2][0].Offset != 0 {
		t.Fatalf("a text inside a chunk: %v", got[2])
	}
	// a cut where the white space went: one space joins the two sides
	got = findInChunks(chunksOf(nil, "the late", "pickup slot"), foldText("late pickup"))
	if len(got[0]) != 1 || got[0][0] != (cmi.TextHit{Offset: 4, Length: 4}) || len(got[1]) != 0 {
		t.Fatalf("%v", got)
	}
	// the characters a seam match used are not matched again by the next chunk
	got = findInChunks(hardCut(chunksOf(nil, "xaa", "abab")), foldText("aab"))
	if len(got[0]) != 1 || len(got[1]) != 0 {
		t.Fatalf("%v", got)
	}
	// a seam match that would overlap a hit inside the chunk it begins in is not a second hit
	got = findInChunks(hardCut(chunksOf(nil, "aaaa", "abxx")), foldText("aaa"))
	if len(got[0]) != 1 || got[0][0].Offset != 0 || len(got[1]) != 0 {
		t.Fatalf("%v", got)
	}
	// the characters a seam match used are not a hit of the next chunk again
	got = findInChunks(hardCut(chunksOf(nil, "xab", "abab")), foldText("aba"))
	if len(got[0]) != 1 || got[0][0] != (cmi.TextHit{Offset: 1, Length: 2}) || len(got[1]) != 0 {
		t.Fatalf("%v", got)
	}
	// a cut that is not a hard cut is a line or a paragraph: one space, never nothing - "firstMonday" is not in "on the first" and
	// "Monday of the month", nor 附加费 in 本条规定附加 and 费用另计 (D77)
	got = findInChunks(chunksOf(nil, "on the first", "Monday of the month."), foldText("firstMonday"))
	if len(got) != 0 {
		t.Fatalf("joined with nothing: %v", got)
	}
	got = findInChunks(chunksOf(nil, "on the first", "Monday of the month."), foldText("first Monday"))
	if len(got[0]) != 1 || len(got[1]) != 0 {
		t.Fatalf("joined with one space: %v", got)
	}
	if got := findInChunks(chunksOf(nil, "本条规定附加", "费用另计。"), foldText("附加费")); len(got) != 0 {
		t.Fatalf("a Chinese paragraph end and the next paragraph start: %v", got)
	}
	if got := findInChunks(hardCut(chunksOf(nil, "本条规定附加", "费用另计。")), foldText("附加费")); len(got[0]) != 1 {
		t.Fatalf("a hard cut in the middle of a phrase: %v", got)
	}
	// two seams do not give a text the characters of a chunk twice (the chunks zab | ab | abz hold abab once)
	got = findInChunks(hardCut(chunksOf(nil, "zab", "ab", "abz")), foldText("abab"))
	if n := len(got[0]) + len(got[1]) + len(got[2]); n != 1 {
		t.Fatalf("%d hits: %v", n, got)
	}
	// chunks of different sections, and rows of a table, are not one run of text
	if got := findInChunks(chunksOf(nil, "取件时")[:1], foldText("取件时间")); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	a, b := chunksOf(&cmi.Locator{Headings: []string{"A"}}, "取件时"), chunksOf(&cmi.Locator{Headings: []string{"B"}}, "间")
	if got := findInChunks(hardCut(append(a, b...)), foldText("取件时间")); len(got) != 0 {
		t.Fatalf("across two sections: %v", got)
	}
	tab := &cmi.Locator{Table: "T"}
	if got := findInChunks(hardCut(chunksOf(tab, "| 取件时", "间 |")), foldText("取件时间")); len(got) != 0 {
		t.Fatalf("across the rows of a table: %v", got)
	}
}

// the work of a find is linear in the text whatever the text is: a document of the largest size that has the worst case for a
// search that starts again at every character takes a moment (D73)
func TestFindIsLinearOnAPathologicalDocument(t *testing.T) {
	const size = 4 << 20
	needle := foldText(strings.Repeat("a", 2999) + "b")
	var chunks []postgres.DocChunk
	for n := 0; n < size; n += 1500 {
		chunks = append(chunks, postgres.DocChunk{Seq: len(chunks), Content: strings.Repeat("a", 1500)})
	}
	start := time.Now()
	if got := findInChunks(chunks, needle); len(got) != 0 {
		t.Fatalf("%d chunks have hits", len(got))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a find over %d bytes took %v", size, d)
	}
	start = time.Now()
	one := strings.Repeat("a", size)
	if got := findAll(one, needle); len(got) != 0 {
		t.Fatal("hit")
	}
	if got := findAll(one, foldText(strings.Repeat("a", 1000))); len(got) != size/1000 {
		t.Fatalf("%d hits", len(got))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a find in one chunk of %d bytes took %v", size, d)
	}
}

func TestAReadQueryFitsAPackageAndIsNotEscaped(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	long := &cmi.Locator{Headings: []string{"A & B <c>"}}
	for i := 0; i < 200; i++ {
		long.Headings = append(long.Headings, strings.Repeat("标题", 10))
	}
	q := readQuery(ReadRequest{DocumentID: id, Locator: long})
	if len(q) > 4096 || !utf8.ValidString(q) || !strings.HasPrefix(q, "read "+id+` {"headings":["A & B <c>"`) {
		t.Fatalf("%d bytes: %.120q", len(q), q)
	}
	if got := readQuery(ReadRequest{DocumentID: id, Locator: &cmi.Locator{Page: 2}}); got != "read "+id+` {"page":2}` {
		t.Fatalf("%q", got)
	}
	if got := readQuery(ReadRequest{DocumentID: id}); got != "read "+id {
		t.Fatalf("%q", got)
	}
}
