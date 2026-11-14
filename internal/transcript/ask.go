package transcript

import (
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// An agent's question to the user and the user's answer become two rows
// linked by wire.Ask: the question row where the tool call is, the answer
// row where its result is. Each comes from its own record, so neither knows
// of the other; the app pairs them (a question row with neither its answer
// row nor anything else after it is still waiting for the user).

// askQuestionRow is the row of a question: a tool row whose text is the
// questions, one per line, for apps that do not read Ask.
func askQuestionRow(tool, id string, items []wire.QuestionItem, t int64) wire.Message {
	qs := make([]string, 0, len(items))
	for _, it := range items {
		if s := strings.TrimSpace(it.Question); s != "" {
			qs = append(qs, s)
		}
	}
	return wire.Message{
		Role: RoleTool, ToolName: tool, Text: clip(strings.Join(qs, "\n"), maxText), Time: t,
		Ask: &wire.Ask{ID: id, Kind: wire.AskQuestion, Questions: items},
	}
}

// askAnswerRow is the row of the answers: a user row listing them, one
// question per line (labelled when there are several).
func askAnswerRow(id string, items []wire.QuestionItem, t int64) wire.Message {
	lines := make([]string, 0, len(items))
	for _, it := range items {
		a := strings.Join(it.Answers, ", ")
		if a == "" {
			a = "(no answer)"
		}
		if len(items) > 1 {
			if l := askItemLabel(it); l != "" {
				a = l + ": " + a
			}
		}
		lines = append(lines, a)
	}
	return wire.Message{
		Role: RoleUser, Text: clip(strings.Join(lines, "\n"), maxText), Time: t,
		Ask: &wire.Ask{ID: id, Kind: wire.AskAnswer, Questions: items},
	}
}

// askDeclinedRow is the row of a question closed without an answer.
func askDeclinedRow(id string, t int64) wire.Message {
	return wire.Message{
		Role: RoleSystem, Text: "Question dismissed", Time: t,
		Ask: &wire.Ask{ID: id, Kind: wire.AskAnswer, Declined: true},
	}
}

func askItemLabel(it wire.QuestionItem) string {
	for _, s := range []string{it.Header, it.Question, it.ID} {
		if s = oneLine(s, maxSummary); s != "" {
			return s
		}
	}
	return ""
}

// typedAnswers appends the non-blank texts to answers.
func typedAnswers(answers []string, texts ...string) []string {
	for _, s := range texts {
		if s = strings.TrimSpace(s); s != "" {
			answers = append(answers, s)
		}
	}
	return answers
}
