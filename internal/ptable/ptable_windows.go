package ptable

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func list() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []Proc
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out = append(out, Proc{
			PID:   int(e.ProcessID),
			PPID:  int(e.ParentProcessID),
			Name:  windows.UTF16ToString(e.ExeFile[:]),
			Start: startTime(e.ProcessID),
		})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return out, nil
}

// startTime is the creation time of pid (zero when it cannot be opened).
// New uses it to tell a real parent from a recycled parent pid.
func startTime(pid uint32) time.Time {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return time.Time{}
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return time.Time{}
	}
	return time.Unix(0, created.Nanoseconds())
}

type osSource struct{}

func open(pid int, access uint32) (windows.Handle, error) {
	return windows.OpenProcess(access, false, uint32(pid))
}

func (osSource) Argv(pid int) ([]string, error) {
	h, err := open(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	line, err := commandLine(h)
	if err != nil {
		return nil, err
	}
	return windows.DecomposeCommandLine(line)
}

// commandLine reads a process's command line through
// ProcessCommandLineInformation (Windows 8.1+), which needs no access to
// the process's memory. The result is a UNICODE_STRING whose Buffer points
// into the result buffer itself.
func commandLine(h windows.Handle) (string, error) {
	size := uint32(4096)
	for range 4 {
		buf := make([]uintptr, (uintptr(size)+ptrSize-1)/ptrSize) // pointer-aligned, as the call requires
		raw := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), uintptr(len(buf))*ptrSize)
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), uint32(len(raw)), &size)
		if err == windows.STATUS_INFO_LENGTH_MISMATCH {
			continue
		}
		if err != nil {
			return "", err
		}
		var us windows.NTUnicodeString
		n := uintptr(binary.LittleEndian.Uint16(raw[unsafe.Offsetof(us.Length):]))
		off := ptrAt(raw, unsafe.Offsetof(us.Buffer)) - uintptr(unsafe.Pointer(&buf[0]))
		if off > uintptr(len(raw)) || n > uintptr(len(raw))-off {
			return "", errors.New("ptable: malformed command line")
		}
		return utf16String(raw[off : off+n]), nil
	}
	return "", errors.New("ptable: command line keeps growing")
}

func (osSource) Exe(pid int) (string, error) {
	h, err := open(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// maxEnvBlock bounds the environment block read from another process.
const maxEnvBlock = 1 << 20

func (osSource) Env(pid int) ([]string, error) {
	h, err := open(pid, windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	params, err := userParams(h)
	if err != nil {
		return nil, err
	}
	var p windows.RTL_USER_PROCESS_PARAMETERS
	addr := ptrAt(params, unsafe.Offsetof(p.Environment))
	size := min(ptrAt(params, unsafe.Offsetof(p.EnvironmentSize)), maxEnvBlock) &^ 1
	block := make([]byte, size)
	if err := readMem(h, addr, block); err != nil {
		return nil, err
	}
	// "KEY=value\0KEY=value\0\0"
	var env []string
	for kv := range strings.SplitSeq(utf16String(block), "\x00") {
		if kv == "" {
			break
		}
		env = append(env, kv)
	}
	return env, nil
}

func (osSource) Cwd(pid int) (string, error) {
	h, err := open(pid, windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	params, err := userParams(h)
	if err != nil {
		return "", err
	}
	var p windows.RTL_USER_PROCESS_PARAMETERS
	var us windows.NTUnicodeString
	off := unsafe.Offsetof(p.CurrentDirectory) + unsafe.Offsetof(p.CurrentDirectory.DosPath)
	n := binary.LittleEndian.Uint16(params[off+unsafe.Offsetof(us.Length):])
	buf := make([]byte, n)
	if err := readMem(h, ptrAt(params, off+unsafe.Offsetof(us.Buffer)), buf); err != nil {
		return "", err
	}
	// Stored with a trailing backslash ("C:\work\"); Clean keeps "C:\".
	return filepath.Clean(utf16String(buf)), nil
}

func (osSource) OpenFiles(int) ([]string, error) { return nil, errors.ErrUnsupported }

const ptrSize = unsafe.Sizeof(uintptr(0))

// userParams returns the raw RTL_USER_PROCESS_PARAMETERS of another
// process (PEB → ProcessParameters). They hold addresses in that process,
// so they are kept as bytes and decoded at the offsets of the x/sys
// definitions instead of being loaded into Go pointer fields, which the
// garbage collector would scan.
func userParams(h windows.Handle) ([]byte, error) {
	var pbi [unsafe.Sizeof(windows.PROCESS_BASIC_INFORMATION{}) / ptrSize]uintptr
	if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation, unsafe.Pointer(&pbi[0]), uint32(unsafe.Sizeof(pbi)), nil); err != nil {
		return nil, err
	}
	var bi windows.PROCESS_BASIC_INFORMATION
	peb := pbi[unsafe.Offsetof(bi.PebBaseAddress)/ptrSize]
	var pp [ptrSize]byte
	var pe windows.PEB
	if err := readMem(h, peb+unsafe.Offsetof(pe.ProcessParameters), pp[:]); err != nil {
		return nil, err
	}
	params := make([]byte, unsafe.Sizeof(windows.RTL_USER_PROCESS_PARAMETERS{}))
	if err := readMem(h, ptrAt(pp[:], 0), params); err != nil {
		return nil, err
	}
	return params, nil
}

func readMem(h windows.Handle, addr uintptr, dst []byte) error {
	if len(dst) == 0 {
		return nil
	}
	var n uintptr
	if err := windows.ReadProcessMemory(h, addr, &dst[0], uintptr(len(dst)), &n); err != nil {
		return err
	}
	if n != uintptr(len(dst)) {
		return errors.New("ptable: short process memory read")
	}
	return nil
}

// ptrAt decodes a pointer-sized little-endian value at off.
func ptrAt(b []byte, off uintptr) uintptr {
	if ptrSize == 8 {
		return uintptr(binary.LittleEndian.Uint64(b[off:]))
	}
	return uintptr(binary.LittleEndian.Uint32(b[off:]))
}

// utf16String decodes little-endian UTF-16 bytes.
func utf16String(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}
