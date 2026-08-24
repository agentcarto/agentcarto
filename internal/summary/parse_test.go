package summary

import (
	"strings"
	"testing"
)

func TestParseReadsTheAnswer(t *testing.T) {
	r, e := Parse("@@SESSION\n全体\n\n@@TURN 1\none\n\n@@TURN 2\ntwo\n", []int{1, 2})
	if e != nil {
		t.Fatal(e)
	}
	if r.Session != "全体" || r.Turns[1] != "one" || r.Turns[2] != "two" {
		t.Fatalf("parsed %+v", r)
	}
	if got := r.Missing([]int{1, 2}); len(got) != 0 {
		t.Errorf("Missing=%v want none", got)
	}
}

// The reason this format was chosen: a model writing prose quotes identifiers
// as it goes and does not escape anything. JSON lost every turn to one stray
// quote; here the characters are just characters.
func TestParseKeepsPunctuationThatWouldBreakJSON(t *testing.T) {
	body := `slugはドット(.)も"-"に置換される非可逆変換。path\to\file と ` + "`code`" + ` と {"k":"v"} を含む。`
	r, e := Parse("@@TURN 1\n"+body+"\n", []int{1})
	if e != nil {
		t.Fatal(e)
	}
	if r.Turns[1] != body {
		t.Errorf("the summary came back changed:\n got %q\nwant %q", r.Turns[1], body)
	}
}

// A summary may run to several lines; everything up to the next marker is one
// summary.
func TestParseKeepsMultiLineSummaries(t *testing.T) {
	r, e := Parse("@@TURN 1\n一行目\n二行目\n\n三行目\n@@TURN 2\n別\n", []int{1, 2})
	if e != nil {
		t.Fatal(e)
	}
	if r.Turns[1] != "一行目\n二行目\n\n三行目" {
		t.Errorf("turn 1 = %q", r.Turns[1])
	}
	if r.Turns[2] != "別" {
		t.Errorf("turn 2 = %q", r.Turns[2])
	}
}

// One malformed marker costs its own section and nothing else. Under JSON the
// whole answer was lost.
func TestParseSurvivesOneBadMarker(t *testing.T) {
	r, e := Parse("@@TURN 1\none\n@@TURN あ\nごみ\n@@TURN 3\nthree\n", []int{1, 2, 3})
	if e != nil {
		t.Fatal(e)
	}
	if r.Turns[1] != "one" || r.Turns[3] != "three" {
		t.Fatalf("a bad marker took its neighbours down: %+v", r.Turns)
	}
	if got := r.Missing([]int{1, 2, 3}); len(got) != 1 || got[0] != 2 {
		t.Errorf("Missing=%v want [2]", got)
	}
}

// Haiku 4.5 wraps its output in a fence despite being told not to.
func TestParseStripsACodeFence(t *testing.T) {
	for _, in := range []string{
		"```\n@@TURN 1\none\n```",
		"```text\n@@TURN 1\none\n```",
		"  ```\n@@TURN 1\none\n```  ",
	} {
		r, e := Parse(in, []int{1})
		if e != nil {
			t.Fatalf("%.20q: %v", in, e)
		}
		if r.Turns[1] != "one" {
			t.Errorf("%.20q parsed to %+v", in, r)
		}
	}
}

// A preamble before the first marker belongs to no turn and is dropped.
func TestParseIgnoresWhatComesBeforeTheFirstMarker(t *testing.T) {
	r, e := Parse("はい、要約します。\n\n@@TURN 1\none\n", []int{1})
	if e != nil {
		t.Fatal(e)
	}
	if r.Turns[1] != "one" {
		t.Errorf("turn 1 = %q", r.Turns[1])
	}
	if strings.Contains(r.Session, "要約します") {
		t.Errorf("a preamble was stored as the session summary: %q", r.Session)
	}
}

// A number the document never described is one the model invented.
func TestParseDropsTurnsThatWereNotAskedAbout(t *testing.T) {
	r, e := Parse("@@TURN 1\none\n@@TURN 9\ninvented\n", []int{1})
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := r.Turns[9]; ok {
		t.Errorf("a turn outside the prompt survived: %+v", r.Turns)
	}
}

// A skipped turn is not an error, but it must be visible as skipped rather than
// stored as "nothing happened here".
func TestMissingReportsTheSkippedTurns(t *testing.T) {
	r, e := Parse("@@TURN 1\none\n@@TURN 3\n   \n", []int{1, 2, 3})
	if e != nil {
		t.Fatal(e)
	}
	got := r.Missing([]int{1, 2, 3})
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("Missing=%v want [2 3] (2 was skipped, 3 came back blank)", got)
	}
}

func TestParseRejectsWhatCannotBeUsed(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"empty", ""},
		{"blanks", "   \n  "},
		{"prose with no marker", "すみません、要約できませんでした。"},
		{"markers for turns nobody asked about", "@@TURN 7\nx\n"},
	}
	for _, c := range cases {
		if _, e := Parse(c.in, []int{1, 2}); e == nil {
			t.Errorf("%s: Parse accepted %q", c.name, c.in)
		}
	}
}

// A session summary with no turns is still worth keeping.
func TestParseKeepsASessionOnlyAnswer(t *testing.T) {
	r, e := Parse("@@SESSION\n全体だけ\n", []int{1})
	if e != nil {
		t.Fatal(e)
	}
	if r.Session != "全体だけ" || len(r.Turns) != 0 {
		t.Fatalf("parsed %+v", r)
	}
}

// The error names what came back, so a failure in a background worker's log can
// be diagnosed without rerunning the call.
func TestParseErrorCarriesTheAnswer(t *testing.T) {
	_, e := Parse("すみません、できません", []int{1})
	if e == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(e.Error(), "すみません") {
		t.Errorf("the error does not show what came back: %v", e)
	}
}
