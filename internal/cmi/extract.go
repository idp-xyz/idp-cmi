package cmi

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The converter of an original writes Markdown with whole-line HTML-comment markers (docs/adr/kb3-converter-format.md in the
// platform repository). The chunker reads page, slide, sheet and table markers (ChunkKnowledge); the others only say
// something about the conversion and are taken out of the text by CleanExtracted, so that they are neither indexed nor shown.
var (
	truncatedMarker = regexp.MustCompile(`(?i)^<!--\s*truncated:\s*(.*?)\s*-->$`)
	noTextMarker    = regexp.MustCompile(`(?i)^<!--\s*no-text:\s*page\s+(\d+)\b.*-->$`)
	dropMarker      = regexp.MustCompile(`(?i)^<!--\s*(notes:\s*slide\s+\d+|skipped:.*|sheet:.*\sempty)\s*-->$`)
	anyComment      = regexp.MustCompile(`^<!--.*-->$`)
)

// MaxNotices bounds the notices recorded for one conversion, and MaxNoticeBytes one of them.
const (
	MaxNotices     = 8
	MaxNoticeBytes = 300
)

// ExtractInfo is what the markers of a converted text say about the conversion.
type ExtractInfo struct {
	Truncated   bool     // the converter left something out (a worksheet over the row or column limit)
	Notices     []string // what it left out, and the pages without a text layer: shown to the one who uploaded the original
	NoTextPages []int    // pages that have no text layer (a scan); they are not in the text
	HasBody     bool     // some text that is not a marker: a conversion with none is a failure
}

// CleanExtracted takes the markers that only describe the conversion out of the text and reports them. The text is returned
// with its other markers (page, slide, sheet, table) in place.
func CleanExtracted(text string) (string, ExtractInfo) {
	var info ExtractInfo
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	note := func(s string) {
		if len(info.Notices) < MaxNotices {
			info.Notices = append(info.Notices, cleanNotice(s))
		}
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case truncatedMarker.MatchString(t):
			info.Truncated = true
			note("Truncated: " + truncatedMarker.FindStringSubmatch(t)[1])
		case noTextMarker.MatchString(t):
			n, _ := strconv.Atoi(noTextMarker.FindStringSubmatch(t)[1])
			info.NoTextPages = append(info.NoTextPages, n)
		case dropMarker.MatchString(t):
		default:
			if t != "" && !anyComment.MatchString(t) {
				info.HasBody = true
			}
			out = append(out, line)
		}
	}
	if n := len(info.NoTextPages); n > 0 {
		shown := info.NoTextPages
		more := ""
		if len(shown) > 20 {
			shown, more = shown[:20], fmt.Sprintf(" and %d more", len(info.NoTextPages)-20)
		}
		ids := make([]string, len(shown))
		for i, p := range shown {
			ids[i] = strconv.Itoa(p)
		}
		note(fmt.Sprintf("%d page(s) have no text layer (pictures or scans) and are not in the text: %s%s", n, strings.Join(ids, ", "), more))
	}
	return strings.Join(out, "\n"), info
}

// cleanNotice makes a conversion's notice fit to show: valid UTF-8, no control characters, one line, bounded.
func cleanNotice(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > MaxNoticeBytes {
		s = cutUTF8(s, MaxNoticeBytes-3) + "..."
	}
	return s
}
