package follow

import (
	"reflect"
	"testing"
)

// The outputs below were captured from tmux next-3.4, zellij 0.44.0 and
// herdr 0.9.1. screen is taken from its manual and source (not installed
// where this was written).

func TestParseTmuxClients(t *testing.T) {
	out := "2768500\t2768417\t%0\tprobe\n2768600\t2768417\t%12\tmy session\n\nbad line\n"
	want := []tmuxClient{
		{clientPID: 2768500, panePID: 2768417, paneID: "%0", session: "probe"},
		{clientPID: 2768600, panePID: 2768417, paneID: "%12", session: "my session"},
	}
	if got := parseTmuxClients([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseZellijClients(t *testing.T) {
	out := "CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n" +
		"1         terminal_1     bun /home/u/.bun/bin/omp\n" +
		"2         terminal_0     N/A            \n" +
		"3         plugin_2       zellij:session-manager\n"
	want := []zellijClient{{"1", "terminal_1"}, {"2", "terminal_0"}, {"3", "plugin_2"}}
	if got := parseZellijClients([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseScreenNumber(t *testing.T) {
	for out, want := range map[string]int{
		"1 (bash)\n":                   1,
		"12 (claude code)":             12,
		"This is window 3 (zsh).\r\n":  3,
		"0 (window 7 is not a number)": 0,
	} {
		got, err := parseScreenNumber([]byte(out))
		if err != nil || got != want {
			t.Errorf("parseScreenNumber(%q) = %d, %v; want %d", out, got, err, want)
		}
	}
	if _, err := parseScreenNumber([]byte("No screen session found.\n")); err == nil {
		t.Error("an answer without a number parsed")
	}
}

func TestDecodeHerdr(t *testing.T) {
	list := `{"id":"cli:pane:list","result":{"panes":[{"agent_status":"unknown","cwd":"/tmp","focused":true,"foreground_cwd":"/tmp","pane_id":"w1:p1","revision":1,"tab_id":"w1:t1","terminal_id":"term_65ca7697e564e1","workspace_id":"w1"},{"focused":false,"pane_id":"w1:p2","tab_id":"w1:t1","workspace_id":"w1"}],"type":"pane_list"}}`
	var panes struct {
		Panes []herdrPane `json:"panes"`
	}
	if err := decodeHerdr([]byte(list), &panes); err != nil {
		t.Fatal(err)
	}
	if want := []herdrPane{{ID: "w1:p1", Focused: true}, {ID: "w1:p2"}}; !reflect.DeepEqual(panes.Panes, want) {
		t.Fatalf("panes %+v", panes.Panes)
	}

	info := `{"id":"cli:pane:process_info","result":{"process_info":{"foreground_process_group_id":2775910,"foreground_processes":[{"argv":["/bin/bash"],"cmdline":"/bin/bash","cwd":"/tmp","name":"bash","pid":2775910},{"argv":["vi","x"],"name":"vi","pid":2775999}],"pane_id":"w2:p1","shell_pid":2775910},"type":"pane_process_info"}}`
	var pi struct {
		ProcessInfo herdrProcessInfo `json:"process_info"`
	}
	if err := decodeHerdr([]byte(info), &pi); err != nil {
		t.Fatal(err)
	}
	if got := pi.ProcessInfo; got.ShellPID != 2775910 || len(got.Foreground) != 2 || got.Foreground[1].PID != 2775999 {
		t.Fatalf("process info %+v", got)
	}

	// herdr exits 0 on failures and reports them in the envelope.
	failed := `{"id":"cli:pane:list","error":{"code":"server_not_running","message":"no herdr server is running at /home/u/.config/herdr/sessions/x/herdr.sock"}}`
	if err := decodeHerdr([]byte(failed), &panes); err == nil || err.Error() != "herdr: server_not_running: no herdr server is running at /home/u/.config/herdr/sessions/x/herdr.sock" {
		t.Fatalf("error envelope: %v", err)
	}
}
