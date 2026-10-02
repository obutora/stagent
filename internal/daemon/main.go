package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
)

// memoryLimit is the soft heap limit unless GOMEMLIMIT says otherwise.
const memoryLimit = 32 << 20

// Main runs `stagent daemon`. A second daemon for the same user exits 0
// right away (the IPC address is already served).
func Main(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("usage: stagent daemon\n\nRuns the session registry / event daemon. Started on demand by holders and bridges.")
		return 0
	}
	log.SetPrefix("stagent daemon: ")
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}
	l, err := paths.Resolve()
	if err != nil {
		log.Print(err)
		return 1
	}
	if err := l.EnsureDirs(); err != nil {
		log.Print(err)
		return 1
	}
	ln, err := ipc.Listen(l.DaemonAddr)
	if errors.Is(err, ipc.ErrInUse) {
		return 0
	}
	if err != nil {
		log.Printf("listen %s: %v", l.DaemonAddr, err)
		return 1
	}
	d, err := New(Options{Layout: l})
	if err != nil {
		ln.Close()
		log.Print(err)
		return 1
	}
	signal.Ignore(syscall.SIGHUP)
	// Keep tmp cleaners away from the socket and RunDir while serving (the
	// holder socket directory too: it may be empty between sessions).
	fresh, stopFresh := context.WithCancel(context.Background())
	defer stopFresh()
	go paths.KeepFresh(fresh, l.RunDir, l.HolderSocketDir(), l.DaemonAddr)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			d.Shutdown()
		case <-d.Done():
		}
	}()
	serveErr := d.Serve(ln)
	d.Shutdown()
	if serveErr != nil {
		log.Print(serveErr)
		return 1
	}
	return 0
}
