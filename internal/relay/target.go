package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// NormalizeRemote is a remote URL as relay.start compares them: scheme,
// user and a trailing ".git" or "/" dropped, the host lower-cased, an
// scp-like host:path read as host/path — so git@github.com:o/r.git,
// ssh://git@github.com/o/r and https://github.com/o/r are the same.
func NormalizeRemote(u string) string {
	s := strings.TrimSpace(u)
	hostOf := func(host string) string {
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		return strings.ToLower(host)
	}
	switch {
	case strings.Contains(s, "://"):
		s = s[strings.Index(s, "://")+3:]
		host, rest, _ := strings.Cut(s, "/")
		s = hostOf(host) + "/" + rest
	case strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || filepath.VolumeName(s) != "":
		// a local path
	default:
		colon, slash := strings.IndexByte(s, ':'), strings.IndexByte(s, '/')
		if colon > 0 && (slash < 0 || colon < slash) {
			s = hostOf(s[:colon]) + "/" + s[colon+1:] // scp-like host:path
		}
	}
	for {
		t := strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
		if t == s {
			return s
		}
		s = t
	}
}

// MatchRemote is the name of the remote of the checkout repo whose fetch
// URL is url (NormalizeRemote), "" when none is.
func MatchRemote(ctx context.Context, env []string, repo, url string) string {
	want := NormalizeRemote(url)
	if want == "" {
		return ""
	}
	out, err := git(ctx, env, repo, "remote", "-v")
	if err != nil {
		return ""
	}
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) == 3 && f[2] == "(fetch)" && NormalizeRemote(f[1]) == want {
			return f[0]
		}
	}
	return ""
}

// CloneDir is where relay.start's clone of url goes without a dir: in
// parent, the URL's last path segment without ".git", with -2, -3, …
// added while that exists.
func CloneDir(parent, url string) string {
	name := path.Base(strings.ReplaceAll(NormalizeRemote(url), `\`, "/"))
	name = strings.TrimSuffix(name, ".git")
	if name == "" || name == "." || name == "/" {
		name = "repo"
	}
	dir := filepath.Join(parent, name)
	for i := 2; exists(dir); i++ {
		dir = filepath.Join(parent, name+"-"+strconv.Itoa(i))
	}
	return dir
}

// Clone runs `git clone url dir` with no credential prompt nobody would
// see, within its limit, removing what it made when it fails.
func Clone(ctx context.Context, env []string, url, dir string) error {
	made := !exists(dir)
	env = append(append([]string(nil), env...), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	ctx, cancel := task.WithLimit(ctx, env, task.LimitRelayClone)
	defer cancel()
	if _, err := task.Git(ctx, env, filepath.Dir(dir), "clone", "--quiet", url, dir); err != nil {
		if made {
			os.RemoveAll(dir)
		} else {
			emptyDir(dir)
		}
		return gitErr(err)
	}
	return nil
}

// emptyDir removes what is in dir.
func emptyDir(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// IsEmptyOrAbsent tells whether dir does not exist or is an empty
// directory.
func IsEmptyOrAbsent(dir string) bool {
	st, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || !st.IsDir() {
		return false
	}
	ents, err := os.ReadDir(dir)
	return err == nil && len(ents) == 0
}

// modelShape is relay.start's model: one argument on every OS.
var modelShape = regexp.MustCompile(`^[A-Za-z0-9._:/@+\[\]][A-Za-z0-9._:/@+\[\]-]{0,199}$`)

// ValidModel tells whether model may be relay.start's model.
func ValidModel(model string) bool { return modelShape.MatchString(model) }

// Instruction is the agent's fixed first prompt: the user's own is in the
// document. On Windows a path holding % (cmd.exe would expand it) is named
// by the environment variable instead.
func Instruction(document, goos string) string {
	if goos == "windows" && strings.Contains(document, "%") {
		return "Read the relay document named by the environment variable " + wire.EnvRelayDocument + " and continue the work."
	}
	return "Read the relay document at " + document + " and continue the work."
}

// AgentArgv is the relay target's agent: [command, model args…, prompt
// args…].
func AgentArgv(id, model, document, goos string) ([]string, error) {
	h, ok := harness.Lookup(id)
	if !ok || id == wire.HarnessOther {
		return nil, fmt.Errorf("harness must be an agent harness, not %q", id)
	}
	argv := []string{h.Command}
	if model != "" {
		argv = append(argv, h.ModelArgs(model)...)
	}
	return append(argv, h.PromptArgs(Instruction(document, goos))...), nil
}

// RelayBranch is the new task's branch when relay.start names none: the
// source's branch when the checkout has no local branch of that name,
// else <branch>-relay, <branch>-relay-2, …; on a detached HEAD
// relay-<first 8 of the relay id>. taken tells whether a local branch
// exists.
func RelayBranch(id, source string, taken func(string) bool) string {
	base := source
	if base == "" {
		base = "relay-" + id[:8]
	} else if !taken(base) {
		return base
	} else {
		base += "-relay"
	}
	name := base
	for i := 2; taken(name); i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// NameOf is a task name made from a branch: each / made -, cut to 64
// characters, the characters a name may not have made -, no leading . or
// -.
func NameOf(branch string) string {
	var b strings.Builder
	for _, r := range branch {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" \t\n/\\:*?\"<>|", r) {
			r = '-'
		}
		b.WriteRune(r)
	}
	s := strings.TrimLeft(b.String(), ".-")
	for utf8.RuneCountInString(s) > 64 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	if s == "" {
		s = "relay"
	}
	return s
}

// MemoRequest is the hand-off memo request typed into the source agent.
func MemoRequest(lang string) string {
	if lang == wire.LangJa {
		return "この会話の作業を、これから別の agent が引き継ぎます。引き継ぎのメモを、この返事の本文として書いてください。目的、終わったこと、残っていること、気をつけること、次の一手を入れてください。ファイルは作らず、何も変えないでください。"
	}
	return "Another agent is about to take over the work of this conversation. Write a hand-off memo as the text of this reply: the goal, what is done, what is left, what to watch out for, and the next step. Do not create or change any file."
}
