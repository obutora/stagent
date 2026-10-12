package ios

import (
	"slices"
	"testing"
)

func TestParseDevicesListsBootedIOSSimulatorsOnly(t *testing.T) {
	out := []byte(`{"devices": {
  "com.apple.CoreSimulator.SimRuntime.iOS-27-0": [
    {"udid": "C", "name": "iPhone 18 Pro", "state": "Booted", "isAvailable": true},
    {"udid": "D", "name": "iPhone Air", "state": "Shutdown", "isAvailable": true}
  ],
  "com.apple.CoreSimulator.SimRuntime.iOS-26-2": [
    {"udid": "B", "name": "iPhone 17", "state": "Booted", "isAvailable": true, "dataPath": "/Users/u/Library/Developer/CoreSimulator/Devices/B/data"},
    {"udid": "A", "name": "iPad (A16)", "state": "Booted", "isAvailable": true},
    {"udid": "E", "name": "iPhone 16e", "state": "Shutting Down", "isAvailable": true}
  ],
  "com.apple.CoreSimulator.SimRuntime.watchOS-11-0": [
    {"udid": "W", "name": "Apple Watch Series 11", "state": "Booted", "isAvailable": true}
  ],
  "com.apple.CoreSimulator.SimRuntime.xrOS-26-0": []
}}`)
	got, err := parseDevices(out)
	if err != nil {
		t.Fatal(err)
	}
	const ios26, ios27 = "com.apple.CoreSimulator.SimRuntime.iOS-26-2", "com.apple.CoreSimulator.SimRuntime.iOS-27-0"
	want := []simulator{
		{UDID: "A", Name: "iPad (A16)", Runtime: "iOS 26.2", RuntimeID: ios26},
		{UDID: "B", Name: "iPhone 17", Runtime: "iOS 26.2", RuntimeID: ios26, DataPath: "/Users/u/Library/Developer/CoreSimulator/Devices/B/data"},
		{UDID: "C", Name: "iPhone 18 Pro", Runtime: "iOS 27.0", RuntimeID: ios27},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseDevices = %+v, want %+v", got, want)
	}
}

func TestParseDevicesRejectsOtherOutput(t *testing.T) {
	if _, err := parseDevices([]byte("xcrun: error: unable to find utility")); err == nil {
		t.Fatal("parseDevices accepted text")
	}
	got, err := parseDevices([]byte(`{"devices": {}}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("no devices: %v, %v", got, err)
	}
}
