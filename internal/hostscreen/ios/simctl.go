package ios

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// iosRuntime prefixes the simctl runtime identifiers of iOS (iPhone and
// iPad); watchOS, tvOS and visionOS simulators are not listed.
const iosRuntime = "com.apple.CoreSimulator.SimRuntime.iOS-"

// simulator is one booted iOS simulator of `xcrun simctl list devices
// --json`: DataPath is its data directory, RuntimeID simctl's identifier
// of its runtime (text input reads both, see simctlHost).
type simulator struct {
	UDID, Name, Runtime string
	DataPath, RuntimeID string
}

// parseDevices returns the booted iOS simulators of `xcrun simctl list
// devices --json`, ordered by name, then runtime.
func parseDevices(out []byte) ([]simulator, error) {
	var list struct {
		Devices map[string][]struct {
			UDID     string `json:"udid"`
			Name     string `json:"name"`
			State    string `json:"state"`
			DataPath string `json:"dataPath"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	var sims []simulator
	for runtime, devices := range list.Devices {
		version, ok := strings.CutPrefix(runtime, iosRuntime)
		if !ok {
			continue
		}
		for _, d := range devices {
			if d.State != "Booted" || d.UDID == "" {
				continue
			}
			sims = append(sims, simulator{UDID: d.UDID, Name: d.Name, Runtime: "iOS " + strings.ReplaceAll(version, "-", "."), DataPath: d.DataPath, RuntimeID: runtime})
		}
	}
	slices.SortFunc(sims, func(a, b simulator) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Runtime, b.Runtime), cmp.Compare(a.UDID, b.UDID))
	})
	return sims, nil
}

// items are sims as listed: connectable when idb_companion is installed.
func items(sims []simulator, companion bool) []wire.HostScreenSimulatorItem {
	res := make([]wire.HostScreenSimulatorItem, 0, len(sims))
	for _, s := range sims {
		it := wire.HostScreenSimulatorItem{UDID: s.UDID, Name: s.Name, Runtime: s.Runtime, Connectable: companion}
		if !companion {
			it.Reason = wire.ReasonCompanionNotFound
		}
		res = append(res, it)
	}
	return res
}
