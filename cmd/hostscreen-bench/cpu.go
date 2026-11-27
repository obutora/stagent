package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// cpuTime is the CPU time (user + system) used so far by pids and their
// direct children (idb_companion, adb), read with ps — the one way that
// works on a running process on both macOS and Linux without cgo. macOS
// prints hundredths of a second, procps whole seconds.
func cpuTime(pids []int) (time.Duration, error) {
	all := append([]int(nil), pids...)
	for _, p := range pids {
		out, _ := exec.Command("pgrep", "-P", strconv.Itoa(p)).Output()
		for _, f := range strings.Fields(string(out)) {
			if c, err := strconv.Atoi(f); err == nil {
				all = append(all, c)
			}
		}
	}
	if len(all) == 0 {
		return 0, nil
	}
	list := make([]string, len(all))
	for i, p := range all {
		list[i] = strconv.Itoa(p)
	}
	out, err := exec.Command("ps", "-o", "time=", "-p", strings.Join(list, ",")).Output()
	if err != nil && len(out) == 0 {
		return 0, fmt.Errorf("ps: %w", err)
	}
	var total time.Duration
	for _, f := range strings.Fields(string(out)) {
		d, err := parsePSTime(f)
		if err != nil {
			return 0, err
		}
		total += d
	}
	return total, nil
}

// parsePSTime reads ps's time column: [[dd-]hh:]mm:ss[.ss].
func parsePSTime(s string) (time.Duration, error) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("ps time %q", s)
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("ps time %q", s)
	}
	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("ps time %q", s)
	}
	total := time.Duration(sec*float64(time.Second)+0.5) + time.Duration(days)*24*time.Hour
	unit := time.Minute
	for i := len(parts) - 2; i >= 0; i-- {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0, fmt.Errorf("ps time %q", s)
		}
		total += time.Duration(n) * unit
		unit *= 60
	}
	return total, nil
}
