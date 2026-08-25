package search

import (
	"strings"
	"testing"
)

func TestSummaryHitsFindsTheTurnAndCutsASnippet(t *testing.T) {
	texts := map[int]string{
		0: "PNG の減色まわりを一日かけて詰めたセッション。",
		2: "パレット生成を median cut に差し替えた。",
		5: "テストを追加して pngcrush との差を測った。",
	}
	hits, _ := SummaryHits(texts, NewQuery("パレット"), HitOptions{})
	if len(hits) != 1 {
		t.Fatalf("hits=%+v want the one summary that holds the query", hits)
	}
	if hits[0].Turn != 2 {
		t.Errorf("turn=%d want 2 — a hit has to be followable with `show --turns`", hits[0].Turn)
	}
	if !strings.Contains(hits[0].Snippet, "パレット") {
		t.Errorf("snippet does not carry the match: %q", hits[0].Snippet)
	}
	// The session summary is turn 0, which is what a document keys it by.
	if hits, _ := SummaryHits(texts, NewQuery("減色"), HitOptions{}); len(hits) != 1 || hits[0].Turn != 0 {
		t.Errorf("the session summary should come back as turn 0: %+v", hits)
	}
}

// A caller with no conversation to check the turn ids against passes the session
// summary alone, and then there is no turn to point at — only the one line that
// says what the session was. That is still worth finding.
func TestSummaryHitsOnTheSessionSummaryAlone(t *testing.T) {
	hits, _ := SummaryHits(map[int]string{0: "PNG の減色"}, NewQuery("png"), HitOptions{})
	if len(hits) != 1 || hits[0].Turn != 0 {
		t.Fatalf("hits=%+v", hits)
	}
	// Matching folds case, the same way the rest of the search does.
	if !strings.Contains(hits[0].Snippet, "PNG") {
		t.Errorf("the snippet lost the original casing: %q", hits[0].Snippet)
	}
}

// A snippet is cut in runes, not bytes: a Japanese summary cut on a byte offset
// comes back as broken text.
func TestSummaryHitsCutsInRunes(t *testing.T) {
	long := strings.Repeat("あ", 400) + "めじあんかっと" + strings.Repeat("い", 400)
	hits, _ := SummaryHits(map[int]string{1: long}, NewQuery("めじあんかっと"), HitOptions{Context: 10})
	if len(hits) != 1 {
		t.Fatalf("hits=%+v", hits)
	}
	got := hits[0].Snippet
	if !strings.Contains(got, "めじあんかっと") {
		t.Fatalf("the match itself was cut away: %q", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Errorf("a cut snippet should say it was cut: %q", got)
	}
	if n := len([]rune(got)); n > 40 {
		t.Errorf("snippet is %d runes with --context 10: %q", n, got)
	}
	if !isValidUTF8(got) {
		t.Errorf("the snippet is not valid text: %q", got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestSummaryHitsOnNothing(t *testing.T) {
	for _, c := range []struct {
		name  string
		texts map[int]string
		q     Query
	}{
		{"no summaries", nil, NewQuery("x")},
		{"empty query", map[int]string{0: "何か"}, NewQuery("")},
		{"blank summary", map[int]string{0: ""}, NewQuery("x")},
		{"no match", map[int]string{0: "何か"}, NewQuery("ほかの何か")},
	} {
		if got, total := SummaryHits(c.texts, c.q, HitOptions{}); len(got) != 0 || total != 0 {
			t.Errorf("%s: hits=%+v total=%d", c.name, got, total)
		}
	}
}

// The cap keeps the newest, the way it does for a conversation: a session's
// later turns are what a reader is usually after.
func TestSummaryHitsRespectsMax(t *testing.T) {
	texts := map[int]string{0: "png", 1: "png", 2: "png", 3: "png"}
	hits, total := SummaryHits(texts, NewQuery("png"), HitOptions{Max: 2})
	if len(hits) != 2 || hits[0].Turn != 2 || hits[1].Turn != 3 {
		t.Errorf("hits=%+v want the last two turns", hits)
	}
	// The total is what was found, not what is shown: a caller reporting it as
	// "n hits" must not have the cap fold into the number.
	if total != 4 {
		t.Errorf("total=%d want 4 — the cap must not change the count", total)
	}
}

// The index does not hold generated summaries, so a query has to be matched
// against them directly — by the same rules, so that a search does not find a
// session by its summary and then report no hits in it.
func TestMatchesText(t *testing.T) {
	q := NewQuery("median cut")
	if !q.MatchesText("パレット生成を Median Cut に差し替えた") {
		t.Error("matching does not fold case the way the rest of the search does")
	}
	if q.MatchesText("") {
		t.Error("empty text matched")
	}
	if NewQuery("").MatchesText("何か") {
		t.Error("an empty query matched")
	}
	// A pattern is matched as a pattern here too.
	re, err := NewRegexpQuery("median ?cut")
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchesText("mediancut に差し替えた") {
		t.Error("a regexp query is not applied to summary text")
	}
}
