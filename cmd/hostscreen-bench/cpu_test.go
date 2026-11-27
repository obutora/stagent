package main

import (
	"testing"
	"time"
)

func TestPSTimeFormatsOfMacOSAndLinux(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:01.23":    1230 * time.Millisecond, // macOS
		"12:03.50":   12*time.Minute + 3500*time.Millisecond,
		"1:02:03.00": time.Hour + 2*time.Minute + 3*time.Second,
		"00:00:07":   7 * time.Second, // procps
		"2-01:00:00": 49 * time.Hour,
	} {
		got, err := parsePSTime(in)
		if err != nil || got != want {
			t.Errorf("parsePSTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parsePSTime("soon"); err == nil {
		t.Error("parsePSTime(soon) succeeded")
	}
}
