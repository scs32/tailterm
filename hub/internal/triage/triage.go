// Package triage compares work items for the backlog triage suggestions
// (wi_de078c0ecd9c846b c4): likely duplicates, items a release already
// delivered and items with no recent activity. It only scores text; nothing
// here reads or changes a record.
package triage

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Defaults for the suggestions (owner decision D4).
const (
	TitleThreshold    = 0.6
	CriteriaThreshold = 0.7
	DefaultStaleDays  = 7
)

var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true,
	"for": true, "from": true, "in": true, "into": true, "is": true, "it": true, "its": true, "no": true,
	"not": true, "of": true, "on": true, "or": true, "so": true, "the": true, "that": true, "this": true,
	"to": true, "when": true, "with": true, "without": true,
}

// Tokens returns the distinct lowercase words of s, without stopwords or
// single characters, sorted.
func Tokens(s string) []string {
	seen := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(word)) < 2 || stopwords[word] {
			continue
		}
		seen[word] = true
	}
	out := make([]string, 0, len(seen))
	for word := range seen {
		out = append(out, word)
	}
	sort.Strings(out)
	return out
}

// Jaccard is |a∩b| / |a∪b| over two token sets; two empty sets score 0.
func Jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, t := range a {
		set[t] = true
	}
	both, union := 0, len(set)
	for _, t := range b {
		if set[t] {
			both++
		} else {
			union++
		}
	}
	return float64(both) / float64(union)
}

// Similarity scores two texts by their tokens.
func Similarity(a, b string) float64 { return Jaccard(Tokens(a), Tokens(b)) }

var criterionLine = regexp.MustCompile(`(?i)^\s*(?:[-*]\s*)?(?:\*\*)?[a-z]{1,2}\d{1,3}(?:\s*\([^)]*\))?(?:\*\*)?\s*[:.)](?:\*\*)?\s*(.+)$`)

// Criteria returns the text of the numbered acceptance criteria in an item
// description ("- c1: …", "a2 (c1): …", "**b3.** …"), in order.
func Criteria(description string) []string {
	var out []string
	for _, line := range strings.Split(description, "\n") {
		if m := criterionLine.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

// CriteriaSimilarity scores the combined criteria of two descriptions; it is
// 0 when either has none.
func CriteriaSimilarity(a, b string) float64 {
	ca, cb := Criteria(a), Criteria(b)
	if len(ca) == 0 || len(cb) == 0 {
		return 0
	}
	return Similarity(strings.Join(ca, "\n"), strings.Join(cb, "\n"))
}

// ReleaseRow is one row of a markdown release-record table.
type ReleaseRow struct {
	Cells   []string
	ItemIDs []string
	Text    string // the row's cells without Markdown code marks, joined
}

var itemID = regexp.MustCompile(`\bwi_[0-9a-f]{16}\b`)

// ReleaseRows parses the body rows of every markdown table in a release
// record. Header and separator rows are skipped.
func ReleaseRows(markdown string) []ReleaseRow {
	var rows []ReleaseRow
	header := true
	for _, line := range strings.Split(markdown, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			header = true
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if isSeparator(cells) {
			header = false
			continue
		}
		if header {
			continue // the row above the separator
		}
		text := strings.ReplaceAll(strings.Join(cells, " "), "`", "")
		rows = append(rows, ReleaseRow{Cells: cells, ItemIDs: itemID.FindAllString(line, -1), Text: text})
	}
	return rows
}

func isSeparator(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, ":- ") != "" {
			return false
		}
	}
	return len(cells) > 0
}
