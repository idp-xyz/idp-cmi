package context

import (
	"strings"
	"testing"
	"unicode/utf8"

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
		w := window(text, hit, max)
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
	w := window(text, hit, 1500)
	before, after := strings.Index(w, "needle"), len(w)-strings.Index(w, "needle")-6
	if before < 500 || after < 500 {
		t.Errorf("the hit is not in the middle of the window: %d bytes before, %d after", before, after)
	}
	// a hit longer than the window shows its beginning, still whole characters
	long := strings.Repeat("字", 600)
	w = window(long, cmi.TextHit{Offset: 0, Length: 600}, 100)
	if !utf8.ValidString(w) || len(strings.Trim(w, "…")) > 100 || !strings.HasSuffix(w, "…") {
		t.Errorf("a long hit: %q", w)
	}
	// a text that fits is whole
	if w := window("short text", cmi.TextHit{Offset: 2, Length: 3}, 1500); w != "short text" {
		t.Errorf("%q", w)
	}
}

func TestFoldTextKeepsWhereEachCharacterWas(t *testing.T) {
	f := foldFull("A \n b")
	if string(f.runes) != "a b" || f.start[1] != 1 || f.end[1] != 4 || f.start[2] != 4 {
		t.Fatalf("%+v", f)
	}
}
