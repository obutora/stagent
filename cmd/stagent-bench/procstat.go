package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// procSample is the cumulative CPU use of one process (all threads) read
// from Linux /proc. Other platforms yield ok=false and the bench reports
// the process metrics as n/a.
type procSample struct {
	ok    bool
	cpuNs int64 // time on CPU
	runs  int64 // times scheduled onto a CPU (≈ wakeups)
	// rssKiB is the proportional set size (smaps_rollup Pss), falling back
	// to VmRSS. PSS splits the stagent binary's shared text pages across the
	// holders mapping it, so summing holders does not count them 50 times.
	rssKiB int64
	at     time.Time
}

func sampleProc(pid int) procSample {
	s := procSample{at: time.Now()}
	if pid <= 0 {
		return s
	}
	dir := "/proc/" + strconv.Itoa(pid)
	tasks, err := filepath.Glob(dir + "/task/*/schedstat")
	if err != nil || len(tasks) == 0 {
		return s
	}
	for _, t := range tasks {
		b, err := os.ReadFile(t)
		if err != nil {
			continue // thread exited meanwhile
		}
		f := bytes.Fields(b)
		if len(f) < 3 {
			continue
		}
		ns, _ := strconv.ParseInt(string(f[0]), 10, 64)
		runs, _ := strconv.ParseInt(string(f[2]), 10, 64)
		s.cpuNs += ns
		s.runs += runs
	}
	if rollup, err := os.ReadFile(dir + "/smaps_rollup"); err == nil {
		s.rssKiB = kibField(rollup, "Pss:")
	}
	if s.rssKiB == 0 {
		status, err := os.ReadFile(dir + "/status")
		if err != nil {
			return s
		}
		s.rssKiB = kibField(status, "VmRSS:")
	}
	s.ok = true
	return s
}

// kibField returns the kB value of the first line starting with prefix.
func kibField(b []byte, prefix string) int64 {
	for _, line := range bytes.Split(b, []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte(prefix)); ok {
			if f := bytes.Fields(v); len(f) > 0 {
				n, _ := strconv.ParseInt(string(f[0]), 10, 64)
				return n
			}
		}
	}
	return 0
}

// usage is CPU (fraction of one core) and scheduling rate between samples.
type usage struct {
	ok      bool
	cpu     float64 // 0.01 = 1% of one core
	wakeups float64 // per second
	rssKiB  int64   // at the end of the window
}

func between(a, b procSample) usage {
	if !a.ok || !b.ok {
		return usage{}
	}
	sec := b.at.Sub(a.at).Seconds()
	return usage{
		ok:      true,
		cpu:     float64(b.cpuNs-a.cpuNs) / 1e9 / sec,
		wakeups: float64(b.runs-a.runs) / sec,
		rssKiB:  b.rssKiB,
	}
}

// group aggregates the usage of many processes (the holders).
type group struct {
	n                    int
	cpuSum, wakeSum      float64
	rssMaxKiB, rssSumKiB int64
}

func aggregate(us []usage) (g group, ok bool) {
	for _, u := range us {
		if !u.ok {
			continue
		}
		g.n++
		g.cpuSum += u.cpu
		g.wakeSum += u.wakeups
		g.rssSumKiB += u.rssKiB
		g.rssMaxKiB = max(g.rssMaxKiB, u.rssKiB)
	}
	return g, g.n > 0
}
