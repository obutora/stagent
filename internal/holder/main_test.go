package holder

import (
	"os"
	"testing"

	"github.com/obutora/stagent/internal/paths"
)

// --handoff=auto: STAGENT_HANDOFF 0 / 1 decides, other values are ignored;
// without it config.json's disable_handoff decides, else handoff. Plain
// --handoff and no flag do not look at either.
func TestHandoffChoice(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	const unset = "<unset>"
	for _, c := range []struct {
		flag   string // "" = no flag
		env    string
		config string // "" = no config.json
		want   bool
	}{
		{"--handoff=auto", unset, "", true},
		{"--handoff=auto", unset, `{"notify": {}}`, true},
		{"--handoff=auto", unset, `{"disable_handoff": false}`, true},
		{"--handoff=auto", unset, `{"disable_handoff": true}`, false},
		{"--handoff=auto", "0", "", false},
		{"--handoff=auto", "0", `{"disable_handoff": false}`, false},
		{"--handoff=auto", "1", `{"disable_handoff": true}`, true},
		{"--handoff=auto", "yes", `{"disable_handoff": true}`, false},
		{"--handoff=auto", "true", "", true},
		{"--handoff=auto", "", `{"disable_handoff": true}`, false},
		{"--handoff", "0", `{"disable_handoff": true}`, true},
		{"--handoff=false", "1", "", false},
		{"", "1", "", false},
		{"", unset, "", false},
	} {
		os.Remove(l.Config)
		if c.config != "" {
			if err := os.WriteFile(l.Config, []byte(c.config), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv(EnvHandoff, c.env)
		if c.env == unset {
			os.Unsetenv(EnvHandoff)
		}
		var rf runFlags
		args := []string{"--", "claude"}
		if c.flag != "" {
			args = append([]string{c.flag}, args...)
		}
		if err := rf.flagSet().Parse(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got := rf.wantHandoff(l); got != c.want {
			t.Errorf("%q, STAGENT_HANDOFF=%s, config %q: handoff %v, want %v", c.flag, c.env, c.config, got, c.want)
		}
	}

	// Like --handoff, --handoff=auto applies to passthrough sessions only.
	for _, f := range []string{"--handoff", "--handoff=auto"} {
		if code := Main([]string{"--detached", f, "--", "true"}); code != ExitUsage {
			t.Errorf("--detached %s: exit %d, want %d", f, code, ExitUsage)
		}
	}
}
