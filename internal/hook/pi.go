package hook

import (
	"encoding/json"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// piHook: Pi's extension passes the event and payload the usual way. Pi
// sets its process title, which overwrites its arguments in the process
// table on macOS and Linux, so the extension adds its command line
// (process.argv after the script) as argv: a batch run is told by that.
var piHook = hookHarness{
	launch: func(li *launchInfo, payload []byte) {
		var pl struct {
			Argv []string `json:"argv"`
		}
		if json.Unmarshal(payload, &pl) != nil || len(pl.Argv) == 0 {
			return
		}
		if harness.BatchArgs(wire.HarnessPi, append([]string{"pi"}, pl.Argv...)) {
			li.parentBatch = true
		}
	},
}
