package wire

// ---------------------------------------------------------------------------
// Host screen (ホストの画面, ADR 0008): a browser (CDP) an agent drives, an
// Android emulator (gRPC) or an iOS simulator, shown in an app tab over the
// tab's own bridge. Served by the bridge itself on every OS, without a
// capability; the iOS kind only on macOS (CapHostScreenIOS).

const (
	MethodHostScreenList   = "host_screen.list"
	MethodHostScreenStart  = "host_screen.start"
	MethodHostScreenAck    = "host_screen.ack"
	MethodHostScreenPause  = "host_screen.pause"
	MethodHostScreenResume = "host_screen.resume"
	MethodHostScreenStop   = "host_screen.stop"
	MethodHostScreenInput  = "host_screen.input"

	NotifyHostScreenFrame = "host_screen.frame" // HostScreenFrame
	NotifyHostScreenState = "host_screen.state" // HostScreenState
)

// HostScreenMethods are served by the bridge itself.
var HostScreenMethods = map[string]bool{
	MethodHostScreenList: true, MethodHostScreenStart: true, MethodHostScreenAck: true, MethodHostScreenStop: true,
	MethodHostScreenPause: true, MethodHostScreenResume: true, MethodHostScreenInput: true,
}

// Target kinds of HostScreenTarget and keys of host_screen.list's result.
const (
	HostScreenBrowser = "browser"
	HostScreenAndroid = "android"
	HostScreenIOS     = "ios"
)

// host_screen.state values.
const (
	HostScreenLive = "live"
	HostScreenGone = "gone"
)

// Tool names of HostScreenBrowserItem, told apart by the profile path.
const (
	ToolAgentBrowser      = "agent-browser"
	ToolPlaywrightMCP     = "playwright-mcp"
	ToolChromeDevToolsMCP = "chrome-devtools-mcp"
	ToolPlaywright        = "playwright"
)

// CapHostScreenIOS (macOS only): host_screen.list has an ios section and
// host_screen.start serves the ios kind.
const CapHostScreenIOS = "host_screen_ios"

// Codes of HostScreenMissing and reasons of HostScreenSimulatorItem.
const (
	// MissingXcodeNotFound: xcrun cannot find simctl (no Xcode selected).
	MissingXcodeNotFound = "xcode_not_found"
	// MissingSimctlFailed: simctl is there but failed (its error is the
	// message), e.g. Xcode's license is not accepted yet.
	MissingSimctlFailed = "simctl_failed"
	// ReasonCompanionNotFound: idb_companion, which shows a simulator, is
	// not installed.
	ReasonCompanionNotFound = "companion_not_found"
)

// HostScreenSection is one kind's part of host_screen.list's result.
type HostScreenSection[T any] struct {
	Items []T `json:"items"`
	// Missing: the tool that finds this kind is not installed, or failed.
	Missing *HostScreenMissing `json:"missing,omitempty"`
	// Note: why the section may lack targets the person sees running,
	// told even with items.
	Note *HostScreenNote `json:"note,omitempty"`
}

// NoteWSL: the host is a Linux inside Windows' WSL, which sees neither the
// browsers nor the emulators on the Windows side.
const NoteWSL = "wsl"

// HostScreenNote is a remark on a section, whatever it lists.
type HostScreenNote struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HostScreenMissing tells why a section cannot list anything.
type HostScreenMissing struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HostScreenListParams: CDPPorts are ports on 127.0.0.1 the app names by
// hand (each 1–65535), listed when they answer as CDP.
type HostScreenListParams struct {
	CDPPorts []int `json:"cdp_ports,omitempty"`
}

// HostScreenPipeOnly is the Reason of a browser started with
// --remote-debugging-pipe only: it opened no port to connect to.
const HostScreenPipeOnly = "pipe_only"

// HostScreenBrowserItem is one browser with an open CDP port, or one that
// cannot be connected to (Connectable false, Reason says why).
type HostScreenBrowserItem struct {
	PID int `json:"pid,omitempty"`
	// Tool is null when the profile path names none of the known tools.
	Tool     *string `json:"tool"`
	Headless *bool   `json:"headless,omitempty"`
	Port     int     `json:"port,omitempty"`
	// Manual: Port is one of HostScreenListParams.CDPPorts.
	Manual      bool             `json:"manual,omitempty"`
	Connectable bool             `json:"connectable"`
	Reason      string           `json:"reason,omitempty"`
	Pages       []HostScreenPage `json:"pages"`
}

// HostScreenPage is one page target of a browser.
type HostScreenPage struct {
	TargetID string `json:"target_id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

// Codes and reasons of the android section.
const (
	// HostScreenADBNotFound is the android section's missing code: adb is
	// not installed.
	HostScreenADBNotFound = "adb_not_found"
	// HostScreenNoGRPC is the reason of an emulator without a gRPC port
	// (started with -port only, say).
	HostScreenNoGRPC = "no_grpc"
)

// HostScreenEmulatorItem is one running Android emulator: Serial is adb's
// (emulator-<console port>), AVD the virtual device's name.
type HostScreenEmulatorItem struct {
	Serial      string `json:"serial"`
	AVD         string `json:"avd"`
	Connectable bool   `json:"connectable"`
	Reason      string `json:"reason,omitempty"`
}

// HostScreenSimulatorItem is one booted iOS simulator.
type HostScreenSimulatorItem struct {
	UDID string `json:"udid"`
	Name string `json:"name"`
	// Runtime is the readable runtime, e.g. "iOS 26.2".
	Runtime     string `json:"runtime"`
	Connectable bool   `json:"connectable"`
	Reason      string `json:"reason,omitempty"`
}

// HostScreenTarget names what host_screen.start shows: for a browser Port
// and TargetID, for an Android emulator Serial (and AVD, which tells the
// emulator again after a restart changed its serial), for an iOS
// simulator UDID.
type HostScreenTarget struct {
	Kind     string `json:"kind"`
	Port     int    `json:"port,omitempty"`
	TargetID string `json:"target_id,omitempty"`
	Serial   string `json:"serial,omitempty"`
	AVD      string `json:"avd,omitempty"`
	UDID     string `json:"udid,omitempty"`
}

type HostScreenStartParams struct {
	Target HostScreenTarget `json:"target"`
}

type HostScreenAckParams struct {
	Seq int64 `json:"seq"`
}

// HostScreenFrame is one JPEG; Data is its base64 (kept encoded end to
// end: CDP sends it so). Width and Height are the target's size in its own
// coordinates (CSS px for a browser, px for an Android emulator, pt for an
// iOS simulator).
type HostScreenFrame struct {
	Seq    int64  `json:"seq"`
	Data   string `json:"data"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// HostScreenState: State is HostScreenLive or HostScreenGone; Title and
// URL are a page's.
type HostScreenState struct {
	State string `json:"state"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

// HostScreenInput types (host_screen.input's type).
const (
	InputTap      = "tap"
	InputDrag     = "drag"
	InputScroll   = "scroll"
	InputText     = "text"
	InputKey      = "key"
	InputNavigate = "navigate"
)

// HostScreenInput keys (type key): Android's and iOS's buttons.
const (
	KeyBack    = "back"
	KeyHome    = "home"
	KeyRecents = "recents"
)

// HostScreenInput actions (type navigate): a browser's.
const (
	NavigateBack    = "back"
	NavigateForward = "forward"
	NavigateReload  = "reload"
	NavigateURL     = "url"
)

// HostScreenInput is the params of host_screen.input: one operation on
// the target being shown. Points are fractions of the frame (0 at its left
// and top edge, 1 at its right and bottom one); stagent turns them into
// the target's coordinates. Which fields count depends on Type:
//
//	tap      X, Y
//	drag     Points (in order), DurationMS
//	scroll   X, Y, DX, DY (how far the fingers moved, fractions of the
//	         frame's width and height)
//	text     Text, Enter; X, Y the cursor (iOS opens the edit menu there)
//	key      Key
//	navigate Action, URL (for NavigateURL)
type HostScreenInput struct {
	Type       string            `json:"type"`
	X          float64           `json:"x,omitempty"`
	Y          float64           `json:"y,omitempty"`
	Points     []HostScreenPoint `json:"points,omitempty"`
	DurationMS int               `json:"duration_ms,omitempty"`
	DX         float64           `json:"dx,omitempty"`
	DY         float64           `json:"dy,omitempty"`
	Text       string            `json:"text,omitempty"`
	Enter      bool              `json:"enter,omitempty"`
	Key        string            `json:"key,omitempty"`
	Action     string            `json:"action,omitempty"`
	URL        string            `json:"url,omitempty"`
}

// HostScreenPoint is a point of a drag, in fractions of the frame.
type HostScreenPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// HostScreenInput vias (HostScreenInputResult.Via): how an iOS simulator
// took a text (#649).
const (
	// InputViaCmdV: pasted with ⌘V into an app allowed to paste from other
	// apps; keys wait about 3 s after it.
	InputViaCmdV = "cmd_v"
	// InputViaEditMenu: pasted by tapping the edit menu's Paste at the
	// cursor; the app is not allowed to paste from other apps.
	InputViaEditMenu = "edit_menu"
	// InputViaKeys: typed as keys (ASCII only), after the 英数 key.
	InputViaKeys = "keys"
)

// HostScreenInputResult is the result of host_screen.input: empty but for
// a text an iOS simulator took, which tells how (Via) and the name of the
// app in front (App, when known).
type HostScreenInputResult struct {
	Via string `json:"via,omitempty"`
	App string `json:"app,omitempty"`
}
