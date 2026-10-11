package bridge

import (
	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/android"
	"github.com/obutora/stagent/internal/hostscreen/browser"
	"github.com/obutora/stagent/internal/hostscreen/ios"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// Queue keys of the host_screen.* methods. start and stop (and input, which
// must keep its order) share one queue; list has its own so probing CDP
// ports never delays them. ack takes no queue (see dispatchHostScreen).
const (
	targetHostScreen     = "host_screen"
	targetHostScreenList = "host_screen.list"
)

// hostScreenKinds are the kinds of target a bridge on goos shows: browsers
// and Android emulators everywhere, iOS simulators on macOS only (announced
// as wire.CapHostScreenIOS).
func (b *Bridge) hostScreenKinds(goos string) []hostscreen.Kind {
	kinds := []hostscreen.Kind{browser.New(), android.New(b.sessionEnv)}
	if goos == "darwin" {
		// idb_companion is usually in Homebrew's bin, which an SSH exec's
		// PATH lacks.
		kinds = append(kinds, ios.New(func() []string { return ensureOnPath(b.sessionEnv(), "idb_companion", b.l.Home) }))
	}
	return kinds
}

// dispatchHostScreen routes a host_screen.* request (ADR 0008). They show
// what the user's own processes draw and change no setting, so a bridge
// under a coding agent serves them too (ADR 0004), like github.*.
func (b *Bridge) dispatchHostScreen(m *wire.Msg) {
	switch m.Method {
	case wire.MethodHostScreenAck:
		// Answered at once, never behind a slow start or input: a late ack
		// would stall the frames.
		var p wire.HostScreenAckParams
		if err := rpc.Decode(m, &p); err != nil {
			b.replyResult(m.ID, nil, err)
			return
		}
		b.replyResult(m.ID, struct{}{}, b.hostScreen.Ack(p.Seq))
	case wire.MethodHostScreenList:
		b.enqueue(targetHostScreenList, m, func(m *wire.Msg) {
			var p wire.HostScreenListParams
			if err := rpc.Decode(m, &p); err != nil {
				b.replyResult(m.ID, nil, err)
				return
			}
			res, err := b.hostScreen.List(b.ctx, p)
			b.replyResult(m.ID, res, err)
		})
	case wire.MethodHostScreenStart:
		b.enqueue(targetHostScreen, m, b.hostScreenStart)
	case wire.MethodHostScreenStop:
		b.enqueue(targetHostScreen, m, func(m *wire.Msg) {
			b.hostScreen.Stop()
			b.replyResult(m.ID, struct{}{}, nil)
		})
	case wire.MethodHostScreenInput:
		// On start's queue: inputs keep the app's order and never reach a
		// stream being replaced.
		b.enqueue(targetHostScreen, m, func(m *wire.Msg) {
			var in wire.HostScreenInput
			if err := rpc.Decode(m, &in); err != nil {
				b.replyResult(m.ID, nil, err)
				return
			}
			b.replyResult(m.ID, struct{}{}, b.hostScreen.Input(b.ctx, in))
		})
	}
}

// hostScreenStart answers before the new stream's first notification.
func (b *Bridge) hostScreenStart(m *wire.Msg) {
	var p wire.HostScreenStartParams
	if err := rpc.Decode(m, &p); err != nil {
		b.replyResult(m.ID, nil, err)
		return
	}
	err := b.hostScreen.Start(b.ctx, p.Target, func() { b.replyResult(m.ID, struct{}{}, nil) })
	if err != nil {
		b.replyResult(m.ID, nil, err)
	}
}
