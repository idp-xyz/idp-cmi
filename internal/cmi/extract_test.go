package cmi

import (
	"reflect"
	"strings"
	"testing"
)

func TestCleanExtractedTakesOutTheDescribingMarkers(t *testing.T) {
	in := "<!-- page: 1 -->\nFirst page.\n<!-- page: 2 -->\n<!-- no-text: page 2 (a picture) -->\n<!-- page: 3 -->\nThird.\n" +
		"<!-- sheet: Data range: A1:C3 -->\n| row | A | B |\n| --- | --- | --- |\n| 2 | x | y |\n<!-- truncated: sheet Data, rows kept 10000 of 120000 -->\n" +
		"<!-- sheet: Blank empty -->\n<!-- skipped: sheet Old, hidden -->\n<!-- notes: slide 4 -->\nspeaker words\n"
	got, info := CleanExtracted(in)
	for _, gone := range []string{"truncated:", "no-text:", "skipped:", "notes:", "Blank empty"} {
		if strings.Contains(got, gone) {
			t.Fatalf("%q is still in the text:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"<!-- page: 2 -->", "<!-- sheet: Data range: A1:C3 -->", "speaker words"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("%q was taken out:\n%s", kept, got)
		}
	}
	if !info.Truncated || !info.HasBody || !reflect.DeepEqual(info.NoTextPages, []int{2}) {
		t.Fatalf("info %+v", info)
	}
	if len(info.Notices) != 2 || !strings.HasPrefix(info.Notices[0], "Truncated: sheet Data, rows kept 10000 of 120000") || !strings.Contains(info.Notices[1], "1 page(s) have no text layer") {
		t.Fatalf("notices %q", info.Notices)
	}
}

func TestAConversionOfMarkersAloneHasNoBody(t *testing.T) {
	_, info := CleanExtracted("<!-- page: 1 -->\n<!-- no-text: page 1 -->\n\n<!-- page: 2 -->\n   \n")
	if info.HasBody {
		t.Fatal("markers and blank lines are no body")
	}
	if _, info := CleanExtracted("<!-- page: 1 -->\nx"); !info.HasBody {
		t.Fatal("a word is a body")
	}
}

func TestNoticesAreOneBoundedLineWithoutControls(t *testing.T) {
	_, info := CleanExtracted("a\n<!-- truncated: sheet \x07Evil\tName " + strings.Repeat("é", 400) + " -->\n")
	n := info.Notices[0]
	if len(n) > MaxNoticeBytes || strings.ContainsAny(n, "\x07\t\n") {
		t.Fatalf("%q", n)
	}
}
