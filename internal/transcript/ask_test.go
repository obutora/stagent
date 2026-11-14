package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// Rows of an agent's question and its answer (wire.Ask), in the ask
// fixtures:
//
//   - claude-ask.jsonl: a real Claude Code 2.1.290 transcript (one
//     single-select question) with its texts redacted to placeholders.
//   - claude-ask-multi.jsonl: a real Claude Code 2.1.325 session
//     (synthetic questions, cwd, hook path and signature redacted; its
//     records other than user, assistant and system dropped): two
//     questions answered with an option and two of a multi-select, the
//     same two answered with typed text and an option plus typed text of
//     the multi-select, then one question cancelled with Esc.
//   - claude-ask-pending.jsonl: the first two questions of that session
//     as they were on disk while the question was still open.
//   - omp-ask.jsonl: real omp 18.12.6 sessions (synthetic questions, cwd
//     and signatures redacted): two questions at once (typed text, two of
//     a multi-select), one cancelled, one at a time (typed text, then two
//     of a multi-select), one option with a note.
//   - omp-ask-pending.jsonl: the first of those sessions as it was on
//     disk while the question was still open.
//   - codex-ask.jsonl: a real Codex 0.160.1 rollout in Plan mode (synthetic
//     questions; only session_meta, response_item and turn_aborted records
//     kept, reasoning and developer texts dropped or redacted, the cwd
//     redacted): two questions at once (an option with a note, "None of
//     the above" with a note), one left with Esc, one answered with an
//     option, a command's wait interrupted with Esc, one more left with
//     Esc.

func question(tool, id, text string, items ...wire.QuestionItem) wire.Message {
	return wire.Message{Role: RoleTool, ToolName: tool, Text: text,
		Ask: &wire.Ask{ID: id, Kind: wire.AskQuestion, Questions: items}}
}

func answer(id, text string, items ...wire.QuestionItem) wire.Message {
	return wire.Message{Role: RoleUser, Text: text,
		Ask: &wire.Ask{ID: id, Kind: wire.AskAnswer, Questions: items}}
}

func declined(id string) wire.Message {
	return wire.Message{Role: RoleSystem, Text: "Question dismissed",
		Ask: &wire.Ask{ID: id, Kind: wire.AskAnswer, Declined: true}}
}

func user(text string) wire.Message      { return wire.Message{Role: RoleUser, Text: text} }
func assistant(text string) wire.Message { return wire.Message{Role: RoleAssistant, Text: text} }
func codexTool(name, text string) wire.Message {
	return wire.Message{Role: RoleTool, ToolName: name, Text: text}
}

var askFixtureWant = map[string]struct {
	harness string
	want    []wire.Message
}{
	"claude-ask": {wire.HarnessClaude, []wire.Message{
		question("AskUserQuestion", "toolu_01XWZsjyAfwQh2SEHLta6WaC", "question 1",
			wire.QuestionItem{Header: "header 1", Question: "question 1"}),
		// The tool_result's text is not shown; toolUseResult's answers are.
		answer("toolu_01XWZsjyAfwQh2SEHLta6WaC", "label 2",
			wire.QuestionItem{Header: "header 1", Question: "question 1", Answers: []string{"label 2"}}),
	}},
	"claude-ask-multi": {wire.HarnessClaude, []wire.Message{
		user("AskUserQuestion ツールを1回だけ使って、2問を同時に聞いてください。1問目: header「色」、問い「好きな色はどれですか？」、単一選択、選択肢 赤/青/緑。2問目: header「果物」、問い「好きな果物をすべて選んでください」、multiSelect: true、選択肢 りんご/みかん/ぶどう。答えを受け取ったら、その答えを一文で復唱するだけで、ほかのツールは使わないでください。"),
		question("AskUserQuestion", "toolu_012Fk7zjxqrZZg7YdMe3WRLG", "好きな色はどれですか？\n好きな果物をすべて選んでください",
			wire.QuestionItem{Header: "色", Question: "好きな色はどれですか？"},
			wire.QuestionItem{Header: "果物", Question: "好きな果物をすべて選んでください", Multi: true}),
		answer("toolu_012Fk7zjxqrZZg7YdMe3WRLG", "色: 青\n果物: りんご, ぶどう",
			wire.QuestionItem{Header: "色", Question: "好きな色はどれですか？", Answers: []string{"青"}},
			wire.QuestionItem{Header: "果物", Question: "好きな果物をすべて選んでください", Multi: true, Answers: []string{"りんご", "ぶどう"}}),
		assistant("好きな色は青で、好きな果物はりんごとぶどうですね。"),
		user("もう一度 AskUserQuestion ツールを1回だけ使って、さっきと同じ2問（色は単一選択、果物は multiSelect: true）を聞いてください。答えを受け取ったら、その答えを一文で復唱するだけで、ほかのツールは使わないでください。"),
		question("AskUserQuestion", "toolu_0137SMh64WtHVWrVokzhNGVw", "好きな色はどれですか？\n好きな果物をすべて選んでください",
			wire.QuestionItem{Header: "色", Question: "好きな色はどれですか？"},
			wire.QuestionItem{Header: "果物", Question: "好きな果物をすべて選んでください", Multi: true}),
		// Typed text ("Type something") is the answer of a single-select
		// and the last element of a multi-select's.
		answer("toolu_0137SMh64WtHVWrVokzhNGVw", "色: 紫がいい\n果物: みかん, メロンも",
			wire.QuestionItem{Header: "色", Question: "好きな色はどれですか？", Answers: []string{"紫がいい"}},
			wire.QuestionItem{Header: "果物", Question: "好きな果物をすべて選んでください", Multi: true, Answers: []string{"みかん", "メロンも"}}),
		assistant("好きな色は紫で、好きな果物はみかんとメロンですね。"),
		user("もう一度 AskUserQuestion ツールを1回だけ使って、1問だけ聞いてください: header「サイズ」、問い「サイズはどれにしますか？」、単一選択、選択肢 S/M/L。答えがなくても、その旨を一文で返すだけにしてください。"),
		question("AskUserQuestion", "toolu_01Dm2cUnTM4wEd7Tq1kRhWoV", "サイズはどれにしますか？",
			wire.QuestionItem{Header: "サイズ", Question: "サイズはどれにしますか？"}),
		// Esc: the rejected call's result is no answer; the interrupt
		// closes the question.
		{Role: RoleSystem, Text: "Interrupted by user"},
		user("/exit"),
	}},
	"claude-ask-pending": {wire.HarnessClaude, []wire.Message{
		user("AskUserQuestion ツールを1回だけ使って、2問を同時に聞いてください。1問目: header「色」、問い「好きな色はどれですか？」、単一選択、選択肢 赤/青/緑。2問目: header「果物」、問い「好きな果物をすべて選んでください」、multiSelect: true、選択肢 りんご/みかん/ぶどう。答えを受け取ったら、その答えを一文で復唱するだけで、ほかのツールは使わないでください。"),
		// The last row: waiting for the user.
		question("AskUserQuestion", "toolu_012Fk7zjxqrZZg7YdMe3WRLG", "好きな色はどれですか？\n好きな果物をすべて選んでください",
			wire.QuestionItem{Header: "色", Question: "好きな色はどれですか？"},
			wire.QuestionItem{Header: "果物", Question: "好きな果物をすべて選んでください", Multi: true}),
	}},
	"omp-ask": {wire.HarnessOmp, []wire.Message{
		user("ask ツールを1回だけ使って、2問を同時に聞いてください。1問目: id「color」、header「色」、問い「好きな色はどれですか？」、単一選択、選択肢 赤/青/緑。2問目: id「fruit」、header「果物」、問い「好きな果物をすべて選んでください」、複数選択(multi: true)、選択肢 りんご/みかん/ぶどう。答えを受け取ったら、その答えを一文で復唱するだけで、ほかのツールは使わないでください。"),
		question("ask", "toolu_01KzBusH5PPmArttiryNpP7D", "好きな色はどれですか？\n好きな果物をすべて選んでください",
			wire.QuestionItem{ID: "color", Header: "色", Question: "好きな色はどれですか？"},
			wire.QuestionItem{ID: "fruit", Header: "果物", Question: "好きな果物をすべて選んでください", Multi: true}),
		// omp's results carry no header: the question labels the line.
		answer("toolu_01KzBusH5PPmArttiryNpP7D", "好きな色はどれですか？: 紫がいい\n好きな果物をすべて選んでください: りんご, ぶどう",
			wire.QuestionItem{ID: "color", Question: "好きな色はどれですか？", Answers: []string{"紫がいい"}},
			wire.QuestionItem{ID: "fruit", Question: "好きな果物をすべて選んでください", Multi: true, Answers: []string{"りんご", "ぶどう"}}),
		assistant("好きな色は紫で、好きな果物はりんごとぶどうですね。"),
		user("もう一度 ask ツールを1回だけ使って、1問だけ聞いてください: id「size」、header「サイズ」、問い「サイズはどれにしますか？」、単一選択、選択肢 S/M/L。答えがなくても、その旨を一文で返すだけにしてください。"),
		question("ask", "toolu_018YLw9C28uDSi3EbpBfdXrY", "サイズはどれにしますか？",
			wire.QuestionItem{ID: "size", Header: "サイズ", Question: "サイズはどれにしますか？"}),
		declined("toolu_018YLw9C28uDSi3EbpBfdXrY"),
		user("ask ツールを2回、順番に使ってください（1回に1問ずつ）。1回目: id「size」、header「サイズ」、問い「サイズはどれにしますか？」、単一選択、選択肢 S/M/L。2回目: id「topping」、header「トッピング」、問い「トッピングをすべて選んでください」、multi: true、選択肢 チーズ/バジル/オリーブ。最後に答えを一文で復唱し、ほかのツールは使わないでください。"),
		question("ask", "toolu_01SFkr3U7NC72LrREcYTJYoE", "サイズはどれにしますか？",
			wire.QuestionItem{ID: "size", Header: "サイズ", Question: "サイズはどれにしますか？"}),
		answer("toolu_01SFkr3U7NC72LrREcYTJYoE", "大きめがいい",
			wire.QuestionItem{Question: "サイズはどれにしますか？", Answers: []string{"大きめがいい"}}),
		question("ask", "toolu_01E5etxZsyGssW6xfuRfHdo6", "トッピングをすべて選んでください",
			wire.QuestionItem{ID: "topping", Header: "トッピング", Question: "トッピングをすべて選んでください", Multi: true}),
		answer("toolu_01E5etxZsyGssW6xfuRfHdo6", "チーズ, バジル",
			wire.QuestionItem{Question: "トッピングをすべて選んでください", Multi: true, Answers: []string{"チーズ", "バジル"}}),
		assistant("サイズは「大きめがいい」（S/M/L のどれかは選ばれていません）、トッピングはチーズとバジルです。"),
		user("Use the ask tool to ask me which color I prefer, options Red, Green, Blue. Then reply with my answer and any note I gave, nothing else."),
		question("ask", "toolu_01HGb6A1TwkYwXxc3rDxsb7A", "Which color do you prefer?",
			wire.QuestionItem{ID: "color", Question: "Which color do you prefer?"}),
		answer("toolu_01HGb6A1TwkYwXxc3rDxsb7A", "Green, a bit darker please",
			wire.QuestionItem{Question: "Which color do you prefer?", Answers: []string{"Green", "a bit darker please"}}),
		assistant(`Green. Note: "a bit darker please"`),
	}},
	"omp-ask-pending": {wire.HarnessOmp, []wire.Message{
		user("ask ツールを1回だけ使って、2問を同時に聞いてください。1問目: id「color」、header「色」、問い「好きな色はどれですか？」、単一選択、選択肢 赤/青/緑。2問目: id「fruit」、header「果物」、問い「好きな果物をすべて選んでください」、複数選択(multi: true)、選択肢 りんご/みかん/ぶどう。答えを受け取ったら、その答えを一文で復唱するだけで、ほかのツールは使わないでください。"),
		// The last row: waiting for the user.
		question("ask", "toolu_01KzBusH5PPmArttiryNpP7D", "好きな色はどれですか？\n好きな果物をすべて選んでください",
			wire.QuestionItem{ID: "color", Header: "色", Question: "好きな色はどれですか？"},
			wire.QuestionItem{ID: "fruit", Header: "果物", Question: "好きな果物をすべて選んでください", Multi: true}),
	}},
	"codex-ask": {wire.HarnessCodex, []wire.Message{
		user("まずシェルで `sleep 75` を1回だけ実行してください。そのあと request_user_input を1回だけ使って2問を同時に聞いてください。1問目: id「color」、header「色」、問い「好きな色はどれですか？」、選択肢 赤/青。2問目: id「size」、header「サイズ」、問い「サイズはどれにしますか？」、選択肢 S/M。答えを受け取ったら一文で復唱するだけにして、計画は書かないでください。"),
		assistant("指定どおり、シェルで `sleep 75` を1回実行し、その後に2問をまとめて伺います。"),
		codexTool("exec", "sleep 75"),
		codexTool("exec", "text(await tools.write_stdin({session_id:9353,chars:\"\",yield_time_ms:60000,max_output_tokens:100}));"),
		codexTool("wait", ""),
		codexTool("wait", ""),
		codexTool("wait", ""),
		assistant("`sleep 75` の終了を待っています。"),
		codexTool("exec", "text(await tools.write_stdin({session_id:9353,chars:\"\",yield_time_ms:20000,max_output_tokens:100}));"),
		question("request_user_input", "call_1aa8da38d35d4ad9a4bbfba40b609652", "好きな色はどれですか？\nサイズはどれにしますか？",
			wire.QuestionItem{ID: "color", Header: "色", Question: "好きな色はどれですか？"},
			wire.QuestionItem{ID: "size", Header: "サイズ", Question: "サイズはどれにしますか？"}),
		// Codex's output keys the answers by question id (no order of its
		// own: sorted); a note is typed text.
		answer("call_1aa8da38d35d4ad9a4bbfba40b609652", "color: 青, 少し濃いめ\nsize: None of the above, Lがいい",
			wire.QuestionItem{ID: "color", Answers: []string{"青", "少し濃いめ"}},
			wire.QuestionItem{ID: "size", Answers: []string{"None of the above", "Lがいい"}}),
		assistant("好きな色は少し濃いめの青で、サイズはLですね。"),
		user("もう一度、まずシェルで `sleep 70` を1回だけ実行し、そのあと request_user_input を1回だけ使って1問だけ聞いてください: id「scope」、header「範囲」、問い「どこまで直しますか？」、選択肢 全部/一部。答えがなければ、その旨を一文で返すだけにしてください。"),
		assistant("シェルで `sleep 70` を1回実行し、その後に1問だけ伺います。"),
		codexTool("exec", "sleep 70"),
		codexTool("exec", "text(await tools.write_stdin({session_id:51576,chars:\"\",yield_time_ms:50000,max_output_tokens:100}));"),
		codexTool("wait", ""),
		codexTool("exec", "text(await tools.write_stdin({session_id:51576,chars:\"\",yield_time_ms:1000,max_output_tokens:100}));"),
		assistant("`sleep 70` の終了を待っています。"),
		codexTool("exec", "text(await tools.write_stdin({session_id:51576,chars:\"\",yield_time_ms:15000,max_output_tokens:100}));"),
		question("request_user_input", "call_e1507c4b38a24b5da232da0746d4ece8", "どこまで直しますか？",
			wire.QuestionItem{ID: "scope", Header: "範囲", Question: "どこまで直しますか？"}),
		// Esc on the question interrupts the turn: the call's output is
		// "aborted by user after 21.2s".
		declined("call_e1507c4b38a24b5da232da0746d4ece8"),
		user("もう一度、まずシェルで `sleep 70` を1回だけ実行し、そのあと request_user_input を1回だけ使って1問だけ聞いてください: id「drink」、header「飲み物」、問い「飲み物はどれにしますか？」、選択肢 コーヒー/紅茶。答えを受け取ったら一文で復唱するだけにしてください。"),
		assistant("シェルで `sleep 70` を1回実行し、その後に飲み物を伺います。"),
		codexTool("exec", "sleep 70"),
		codexTool("exec", "text(await tools.write_stdin({session_id:48055,chars:\"\",yield_time_ms:50000,max_output_tokens:100}));"),
		codexTool("wait", ""),
		assistant("`sleep 70` の終了を待っています。"),
		codexTool("exec", "text(await tools.write_stdin({session_id:48055,chars:\"\",yield_time_ms:20000,max_output_tokens:100}));"),
		question("request_user_input", "call_9d63ee32b3784cf593069e9563caceb5", "飲み物はどれにしますか？",
			wire.QuestionItem{ID: "drink", Header: "飲み物", Question: "飲み物はどれにしますか？"}),
		answer("call_9d63ee32b3784cf593069e9563caceb5", "コーヒー",
			wire.QuestionItem{ID: "drink", Answers: []string{"コーヒー"}}),
		assistant("飲み物はコーヒーですね。"),
		user("シェルで `sleep 90` を1回だけ実行し、終わるまで待ってから「終わりました」とだけ返してください。"),
		assistant("`sleep 90` を1回実行し、終了を待ちます。"),
		codexTool("exec", "sleep 90"),
		codexTool("exec", "text(await tools.write_stdin({session_id:21694,chars:\"\",yield_time_ms:50000,max_output_tokens:100}));"),
		user("もう一度、シェルで `sleep 120` を1回だけ実行し、終わるまで待ってから「終わりました」とだけ返してください。"),
		assistant("`sleep 120` を1回実行し、終了を待ちます。"),
		codexTool("exec", "sleep 120"),
		codexTool("exec", "text(await tools.write_stdin({session_id:25430,chars:\"\",yield_time_ms:50000,max_output_tokens:100}));"),
		codexTool("wait", ""),
		// Esc while Codex waited on the command: an interrupted call's
		// output names no tool, but this one's call is wait's, not a
		// question's, so it is no row at all.
		user("request_user_input を1回だけ使って1問だけ聞いてください: id「scope」、header「範囲」、問い「どこまで直しますか？」、選択肢 全部/一部。答えがなければ、その旨を一文で返すだけにしてください。"),
		assistant("範囲を1問だけ伺います。"),
		question("request_user_input", "call_cf1115bf4575472b8e95db1bf55706a5", "どこまで直しますか？",
			wire.QuestionItem{ID: "scope", Header: "範囲", Question: "どこまで直しますか？"}),
		declined("call_cf1115bf4575472b8e95db1bf55706a5"),
	}},
}

func TestParseAskFixtures(t *testing.T) {
	for name, c := range askFixtureWant {
		t.Run(name, func(t *testing.T) {
			p, err := ReadPage(filepath.Join("testdata", name+".jsonl"), c.harness, 0, maxLimit)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]wire.Message, len(p.Messages))
			for i, m := range p.Messages {
				if m.Time == 0 {
					t.Errorf("message without time: %+v", m)
				}
				m.Time = 0
				got[i] = m
			}
			if !reflect.DeepEqual(got, c.want) {
				g, _ := json.MarshalIndent(got, "", " ")
				w, _ := json.MarshalIndent(c.want, "", " ")
				t.Fatalf("messages:\n got %s\nwant %s", g, w)
			}
		})
	}
}

// codexAbortedAt is the offset of the record in codex-ask.jsonl where the
// call callID was interrupted (`aborted by user after <n>s`), and of the
// record after it.
func codexAbortedAt(t *testing.T, data []byte, callID string) (start, end int64) {
	t.Helper()
	i := bytes.Index(data, []byte(`"call_id":"`+callID+`","output":"aborted by user`))
	if i < 0 {
		t.Fatalf("no aborted output of %s", callID)
	}
	s := bytes.LastIndexByte(data[:i], '\n') + 1
	e := i + bytes.IndexByte(data[i:], '\n') + 1
	return int64(s), int64(e)
}

// An interrupted call's output is the question's dismissal when its call is
// a question's, however the record is reached: tailed on its own long after
// the question, or alone on a page whose question is on the next one.
func TestCodexAbortedReadOnItsOwn(t *testing.T) {
	p := filepath.Join("testdata", "codex-ask.jsonl")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		callID string
		want   []wire.Message
	}{
		{"call_e1507c4b38a24b5da232da0746d4ece8", []wire.Message{declined("call_e1507c4b38a24b5da232da0746d4ece8")}},
		{"call_cf1115bf4575472b8e95db1bf55706a5", []wire.Message{declined("call_cf1115bf4575472b8e95db1bf55706a5")}},
		{"call_00e6583f8c0f4e62ac93084fb443ceeb", nil}, // wait's
	} {
		start, end := codexAbortedAt(t, data, c.callID)
		// From that record on: its question is in an earlier read.
		tailed, _, err := ReadFrom(p, wire.HarnessCodex, start)
		if err != nil {
			t.Fatal(err)
		}
		// The newest page ending with that record: its question is on an
		// older page.
		page, err := ReadPage(p, wire.HarnessCodex, end, 1)
		if err != nil {
			t.Fatal(err)
		}
		for how, got := range map[string][]wire.Message{"tailed": tailed, "paged": page.Messages} {
			var rows []wire.Message
			for _, m := range got {
				if m.Ask != nil && m.Ask.Declined && m.Ask.ID == c.callID {
					m.Time = 0
					rows = append(rows, m)
				}
			}
			if !reflect.DeepEqual(rows, c.want) {
				t.Errorf("%s %s: dismissal rows %+v, want %+v", how, c.callID, rows, c.want)
			}
		}
	}
}

func TestSplitClaudeMultiAnswer(t *testing.T) {
	labels := []string{"赤, 濃いめ", `"青"`, "緑"}
	cases := []struct {
		in   string
		want []string
	}{
		{"緑", []string{"緑"}},
		{`"赤, 濃いめ", 緑`, []string{"赤, 濃いめ", "緑"}},   // quoted: holds ", "
		{`"\"青\"", 緑`, []string{`"青"`, "緑"}},       // quoted: holds a quote
		{"紫, 黄", []string{"紫, 黄"}},                 // typed text, not labels
		{"緑, メロンも", []string{"緑", "メロンも"}},         // options, then typed text
		{`緑, "メロン, 多め"`, []string{"緑", "メロン, 多め"}}, // typed text holding ", "
		{"メロンも, 緑", []string{"メロンも, 緑"}},           // typed text comes last
		{`"紫, 黄"`, []string{"紫, 黄"}},               // typed text alone, holding ", "
		{`"赤, 濃いめ`, []string{`"赤, 濃いめ`}},           // unterminated: as typed
		{"", nil},
		{"(notes only)", nil},
	}
	for _, c := range cases {
		if got := claudeAnswerList(json.RawMessage(mustJSON(c.in)), true, labels); !reflect.DeepEqual(got, c.want) {
			t.Errorf("claudeAnswerList(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := claudeAnswerList(json.RawMessage(`["緑","赤, 濃いめ"]`), true, labels); !reflect.DeepEqual(got, []string{"緑", "赤, 濃いめ"}) {
		t.Errorf("array answer = %q", got)
	}
	if got := claudeAnswerList(json.RawMessage(mustJSON("赤, 濃いめ")), false, labels); !reflect.DeepEqual(got, []string{"赤, 濃いめ"}) {
		t.Errorf("single-select answer split: %q", got)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
