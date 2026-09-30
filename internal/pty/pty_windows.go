package pty

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/obutora/stagent/internal/wire"
)

// ErrUnsupported is returned by Start on Windows versions without ConPTY
// (before Windows 10 1809 / Server 2019).
var ErrUnsupported = errors.New("pty: ConPTY is not available (requires Windows 10 1809 / Server 2019 or later)")

// terminateGrace is how long "terminate" waits after Ctrl-C before
// TerminateProcess: console programs have no SIGTERM.
const terminateGrace = 3 * time.Second

var (
	kernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole       = kernel32.NewProc("CreatePseudoConsole")
	procUpdateProcThreadAttribute = kernel32.NewProc("UpdateProcThreadAttribute")
)

type conPTY struct {
	hpc     windows.Handle // pseudo console
	in      windows.Handle // write end: our input to the console
	out     windows.Handle // read end: console output
	process windows.Handle
	pid     int

	done chan struct{}
	code int
	err  error

	closeOnce sync.Once
	outOnce   sync.Once
}

// Start runs argv in dir with env (nil inherits) on a new pseudo console of
// the given size. The program is resolved with exec.LookPath (PATHEXT, so
// `claude` finds claude.cmd); batch files run through %ComSpec% /d /s /c.
func Start(argv []string, dir string, env []string, cols, rows int) (PTY, error) {
	if len(argv) == 0 {
		return nil, errors.New("pty: empty command")
	}
	if procCreatePseudoConsole.Find() != nil {
		return nil, ErrUnsupported
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	app, cmdline, err := commandLine(path, argv[1:])
	if err != nil {
		return nil, err
	}

	// Pipes: we write inR's other end (in) and read outW's other end (out).
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		closeAll(inR, inW)
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}
	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inR, outW, 0, &hpc); err != nil {
		closeAll(inR, inW, outR, outW)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	// The console host holds its own copies; closing ours lets the output
	// pipe report EOF once the pseudo console is closed.
	closeAll(inR, outW)

	pi, err := spawn(hpc, app, cmdline, dir, env)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll(inW, outR)
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	p := &conPTY{hpc: hpc, in: inW, out: outR, process: pi.Process, pid: int(pi.ProcessId), done: make(chan struct{})}
	go p.wait()
	return p, nil
}

func spawn(hpc windows.Handle, app, cmdline, dir string, env []string) (*windows.ProcessInformation, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	// The attribute value is the HPCON itself, not a pointer to it.
	if r, _, e := procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(attrs.List())), 0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(hpc), unsafe.Sizeof(hpc), 0, 0); r == 0 {
		return nil, fmt.Errorf("UpdateProcThreadAttribute: %w", e)
	}
	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	// Zero std handles: without STARTF_USESTDHANDLES a child of a holder
	// whose own std handles are redirected would inherit those instead of
	// the pseudo console.
	si.Flags = windows.STARTF_USESTDHANDLES
	si.ProcThreadAttributeList = attrs.List()

	appPtr, err := windows.UTF16PtrFromString(app)
	if err != nil {
		return nil, err
	}
	cmdPtr, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	var dirPtr *uint16
	if dir != "" {
		if dirPtr, err = windows.UTF16PtrFromString(dir); err != nil {
			return nil, err
		}
	}
	if env == nil {
		env = os.Environ()
	}
	block, err := envBlock(env)
	if err != nil {
		return nil, err
	}
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(appPtr, cmdPtr, nil, nil, false, flags, &block[0], dirPtr, &si.StartupInfo, &pi); err != nil {
		return nil, fmt.Errorf("CreateProcess %s: %w", app, err)
	}
	return &pi, nil
}

// commandLine returns the application to start and its full command line.
func commandLine(path string, args []string) (app, cmdline string, err error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cmd", ".bat":
		// cmd.exe parses the line itself: every argument is quoted so
		// & | < > ( ) ^ stay literal. `"` and line breaks cannot be passed
		// through cmd safely, so they are refused; %VAR% references in
		// arguments are still expanded by cmd.exe.
		comspec := os.Getenv("ComSpec")
		if comspec == "" {
			comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		}
		var b strings.Builder
		b.WriteString(`"`)
		b.WriteString(`"` + path + `"`)
		for _, a := range args {
			if strings.ContainsAny(a, "\"\r\n") {
				return "", "", fmt.Errorf("pty: argument %q cannot be passed to a batch file", a)
			}
			b.WriteString(` "` + a + `"`)
		}
		b.WriteString(`"`)
		return comspec, syscall.EscapeArg(comspec) + ` /d /s /c ` + b.String(), nil
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, syscall.EscapeArg(path))
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return path, strings.Join(parts, " "), nil
}

// envBlock encodes env as a UTF-16 environment block.
func envBlock(env []string) ([]uint16, error) {
	var block []uint16
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			return nil, fmt.Errorf("pty: environment entry contains NUL: %q", kv)
		}
		block = append(block, utf16.Encode([]rune(kv))...)
		block = append(block, 0)
	}
	if len(block) == 0 {
		block = append(block, 0)
	}
	return append(block, 0), nil
}

func (p *conPTY) wait() {
	windows.WaitForSingleObject(p.process, windows.INFINITE)
	var code uint32
	if err := windows.GetExitCodeProcess(p.process, &code); err != nil {
		p.code, p.err = -1, err
	} else {
		p.code = int(int32(code))
	}
	close(p.done)
}

func (p *conPTY) Read(b []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(p.out, b, &n, nil)
	if err != nil {
		// Broken pipe: the pseudo console was closed and drained.
		p.outOnce.Do(func() { windows.CloseHandle(p.out) })
		if n > 0 {
			return int(n), nil
		}
		return 0, io.EOF
	}
	return int(n), nil
}

func (p *conPTY) Write(b []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(p.in, b, &n, nil)
	return int(n), err
}

func (p *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(p.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

func (p *conPTY) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

func (p *conPTY) Pid() int { return p.pid }

func (p *conPTY) Signal(name string) error {
	switch name {
	case wire.SignalInterrupt:
		// The console turns ^C into CTRL_C_EVENT for its processes.
		_, err := p.Write([]byte{0x03})
		return err
	case wire.SignalTerminate, SignalHangup:
		p.Write([]byte{0x03})
		go func() {
			select {
			case <-p.done:
			case <-time.After(terminateGrace):
				windows.TerminateProcess(p.process, 1)
			}
		}()
		return nil
	case wire.SignalKill:
		select {
		case <-p.done:
			return nil
		default:
		}
		return windows.TerminateProcess(p.process, 1)
	}
	return ErrUnknownSignal
}

// Close closes the pseudo console, which ends the program if it still runs
// and makes Read return EOF once the remaining output is drained. Keep
// reading while closing: on older Windows builds ClosePseudoConsole waits
// for the output pipe to drain.
func (p *conPTY) Close() error {
	p.closeOnce.Do(func() {
		windows.ClosePseudoConsole(p.hpc)
		windows.CloseHandle(p.in)
	})
	return nil
}

func closeAll(hs ...windows.Handle) {
	for _, h := range hs {
		if h != 0 && h != windows.InvalidHandle {
			windows.CloseHandle(h)
		}
	}
}
