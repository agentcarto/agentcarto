// Package summary turns a session into a prompt asking a model what happened in
// each of its turns, and turns the answer back into per-turn text.
//
// It holds no I/O of its own: building the prompt and reading the reply are pure
// functions, and running the agent that answers it is behind Generator. The
// split is what makes the part that decides what a summary says testable without
// an API key, a network, or a subprocess.
package summary

import (
	"fmt"
	"sort"
	"strings"

	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// System is the instruction the model runs under. It is deliberately about the
// turn's outcome rather than its request: the CLI already prints what the user
// asked for on the line above (the turn headline is the prompt), so a summary
// that paraphrases the request adds nothing to it.
const System = `あなたはAIコーディングエージェントの会話ログを要約する処理系です。入力は1セッション分のMarkdownで、各ターンは "## Turn N" 見出しの下に USER 発話・ASSISTANT 応答・ツール呼び出しの一覧が並びます。ツール呼び出しは長いものが途中で切られています（末尾の … がその印）。

各ターンについて、そのターンで何が行われ何が分かったかを日本語で要約してください。

制約:
- 依頼文の言い換えを書かない。読み手は依頼文を既に見ている。実際に何をして、何が分かり、何が変わったかを書く。
- 原因を特定したなら原因を、修正したならどこをどう直したかを、結論が出たならその結論を含める。
- ファイル名・関数名・識別子・コミットハッシュは原文のまま残す。
- 長さは各ターンの内容量に見合わせる。上限は設けないが、冗長な説明や自明な補足は書かない。体言止め。丁寧語と主語（「ユーザーが」「Claudeが」）は書かない。
- 入力に無いことを書かない。切られたツール呼び出しの続きを推測しない。
- 加えてセッション全体の要約を1つ作る（長さは同様に内容に見合わせる）。
- さらにセッション全体の見出しを1つ作る。一覧に並べて何のセッションか見分けるための一行で、全角40字以内。列挙にせず、そのセッションが何をしたものかを一つだけ述べる。

出力は次の形式のみ。前後に説明文やコードフェンスを付けない。行頭の @@ で始まる行が区切りで、その次の行から次の区切りまでが本文です。本文は自由なテキストでよく、引用符や記号のエスケープは不要です。

@@HEADLINE
セッション全体の見出し（一行）

@@SESSION
セッション全体の要約

@@TURN 1
ターン1の要約

@@TURN 2
ターン2の要約

入力にあるすべてのターン番号について @@TURN 行を書いてください。

ツールは使わず、与えられた入力だけから答えてください。`

// NodesByTurn maps each turn's number to the id of its terminal node — what the
// summary store checks a stored summary against before showing it. Turn numbers
// are positions along one branch and a rewind renumbers them; the node id is
// what stays attached to the content.
func NodesByTurn(turns []transcript.Turn) map[int]string {
	out := make(map[int]string, len(turns))
	for _, t := range turns {
		if len(t.Nodes) > 0 {
			out[t.Index+1] = t.Nodes[len(t.Nodes)-1]
		}
	}
	return out
}

// Options are the knobs Prompt has. The zero value is what the summarizer uses.
type Options struct {
	// Turns, when set, limits the prompt to these turn numbers (the numbers
	// transcript.Turns shows, 1-based). It is how a session that grew is
	// summarized without paying for the turns already done. Empty means every
	// turn on the path.
	Turns map[int]bool
}

// Prompt renders the session as the document the model is asked about, and
// reports which turn numbers it carries. A caller needs that list because turns
// holding nothing a reader can see (an injected system message and no reply)
// render to nothing and are silently absent — asking for a summary of turn 4
// and getting none is otherwise indistinguishable from the model skipping it.
func Prompt(s domain.Session, c domain.Conversation, turns []transcript.Turn, o Options) (doc string, asked []int) {
	var want []transcript.Turn
	for _, t := range turns {
		if o.Turns != nil && !o.Turns[t.Index+1] {
			continue
		}
		want = append(want, t)
	}
	if len(want) == 0 {
		return "", nil
	}
	// ToolsBrief is the mode that exists for this reader: the whole conversation,
	// tool calls cut to what says which tool ran on what. TaskReports stays off —
	// a subagent's report is prose the size of a turn, and the summary of a turn
	// that delegated is "delegated X", not the delegate's own writeup.
	o2 := transcript.Options{Tools: transcript.ToolsBrief, SessionTurns: len(turns)}
	doc, _ = transcript.Markdown(s, c, want, o2)
	return doc, transcript.RenderedTurns(c, want, s.CWD, o2)
}

// SessionSystem is the instruction for writing a session's own summary from the
// summaries of its turns.
//
// A long session never has all its turns in front of the model at once — it is
// asked about in batches — so the session summary cannot come from any one of
// them. The last batch's answer describes the last few turns, not the session.
// Reading the turn summaries instead costs a fraction of rereading the session
// and sees all of it.
const SessionSystem = `あなたはAIコーディングエージェントの会話ログを要約する処理系です。入力は1セッションの各ターンについて既に作られた要約の一覧です。

これらを読み、セッション全体で何が行われたのかを日本語で1つにまとめてください。

制約:
- 個々のターンの列挙にしない。何を目的に始まり、何が分かり、何が作られ、どこで終わったのかを述べる。
- ファイル名・関数名・識別子・コミットハッシュは原文のまま残す。
- 長さは内容量に見合わせる。上限は設けないが、冗長な説明や自明な補足は書かない。体言止め。丁寧語と主語（「ユーザーが」「Claudeが」）は書かない。
- 入力に無いことを書かない。
- 要約とは別に、セッション全体の見出しを1つ作る。一覧に並べて何のセッションか見分けるための一行で、全角40字以内。列挙にせず、そのセッションが何をしたものかを一つだけ述べる。

出力は次の形式のみ。前後に説明文やコードフェンスを付けない。

@@HEADLINE
セッション全体の見出し（一行）

@@SESSION
セッション全体の要約

ツールは使わず、与えられた入力だけから答えてください。`

// SessionPrompt renders the turn summaries as the document SessionSystem is
// asked about.
func SessionPrompt(s domain.Session, turns map[int]string) string {
	var b strings.Builder
	if title := strings.TrimSpace(s.Title); title != "" {
		b.WriteString("# " + title + "\n\n")
	}
	for _, n := range sortedTurns(turns) {
		fmt.Fprintf(&b, "## Turn %d\n\n%s\n\n", n, strings.TrimSpace(turns[n]))
	}
	return b.String()
}

func sortedTurns(m map[int]string) []int {
	out := make([]int, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
