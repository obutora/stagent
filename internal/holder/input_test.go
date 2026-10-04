package holder

import (
	"reflect"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

func TestComposeInput(t *testing.T) {
	cases := []struct {
		name                     string
		p                        wire.InputParams
		bracketed, appCur, win32 bool
		want                     []inputChunk
	}{
		{"text, keys and submit keep their order; CR follows after a gap",
			wire.InputParams{Text: "ab", Keys: []string{"left", "ctrl-c"}, Submit: true}, false, false, false,
			[]inputChunk{{data: []byte("ab\x1b[D\x03")}, {data: []byte("\r"), delay: submitGap}}},
		{"submit alone is immediate",
			wire.InputParams{Submit: true}, false, false, false,
			[]inputChunk{{data: []byte("\r")}}},
		{"paste is bracketed only when the program enabled it",
			wire.InputParams{Paste: "l1\nl2\r\nl3"}, true, false, false,
			[]inputChunk{{data: []byte("\x1b[200~l1\rl2\rl3\x1b[201~")}}},
		{"paste without bracketed paste mode",
			wire.InputParams{Paste: "l1\nl2"}, false, false, false,
			[]inputChunk{{data: []byte("l1\rl2")}}},
		{"a pasted end marker cannot close the bracket early",
			wire.InputParams{Paste: "x\x1b[201~rm -rf ~\n"}, true, false, false,
			[]inputChunk{{data: []byte("\x1b[200~xrm -rf ~\r\x1b[201~")}}},
		{"cursor keys follow DECCKM",
			wire.InputParams{Keys: []string{"up", "end", "tab"}}, false, true, false,
			[]inputChunk{{data: []byte("\x1bOA\x1bOF\t")}}},
		{"a lone Esc stays a byte outside win32-input-mode",
			wire.InputParams{Keys: []string{"esc"}}, false, false, false,
			[]inputChunk{{data: []byte("\x1b")}}},
		{"win32-input-mode: lone Escs become key events, sequences stay",
			wire.InputParams{Text: "\x1b\x1b", Keys: []string{"up", "esc"}}, false, false, true,
			[]inputChunk{{data: []byte(escKeyEvent + escKeyEvent + "\x1b[A" + escKeyEvent)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := composeInput(tc.p, tc.bracketed, tc.appCur, tc.win32)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	_, err := composeInput(wire.InputParams{Text: "x", Keys: []string{"f13"}}, false, false, false)
	if we, ok := err.(*wire.Error); !ok || we.Code != wire.ErrBadRequest {
		t.Fatalf("unknown key error = %v, want bad_request", err)
	}
}
