package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"slices"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/wire"
)

// Relay trace (跡, PROTOCOL.md "Trace"). The daemon keeps one link per end
// of a relay its conversations took part in, in <root>/state/relays.json,
// the newest relaysKeep of them. A conversation keeps one link per role:
// a newer link of the same conversation and role replaces the older.
// Session.Relay and Conversation.Relay carry the trace of their
// conversation.

// relaysKeep is how many links relays.json keeps (the newest).
const relaysKeep = 1000

// relayLink is one end of a relay as relays.json keeps it: Local is the
// conversation here (no host), which is the relay's Role, Peer the
// conversation at the other end. At (unix ms): when it was first recorded.
type relayLink struct {
	RelayID string        `json:"relay_id"`
	Role    string        `json:"role"`
	At      int64         `json:"at"`
	Local   wire.RelayEnd `json:"local"`
	Peer    wire.RelayEnd `json:"peer"`
}

// relaysFile is the document stored in relays.json.
type relaysFile struct {
	Links []relayLink `json:"links"`
}

func loadRelays(path string) ([]relayLink, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f relaysFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	links := slices.DeleteFunc(f.Links, func(l relayLink) bool {
		return validRelayRecord(wire.RelayRecordParams{RelayID: l.RelayID, Role: l.Role, Local: l.Local, Peer: l.Peer}) != nil
	})
	if len(links) > relaysKeep {
		links = links[len(links)-relaysKeep:]
	}
	return links, nil
}

// saveRelaysLocked writes relays.json (0600, atomically); the trace stays
// in memory when it cannot be written.
func (d *Daemon) saveRelaysLocked() {
	f := relaysFile{Links: d.relays}
	if f.Links == nil {
		f.Links = []relayLink{}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = fsutil.WriteFileAtomic(d.layout.Relays, b)
	}
	if err != nil {
		d.logf("daemon: relays: %v", err)
	}
}

// validRelayID: 32 lowercase hex chars.
func validRelayID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range []byte(id) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func relayIDError(id string) error {
	if !validRelayID(id) {
		return wire.Errorf(wire.ErrBadRequest, "bad relay_id %q", id)
	}
	return nil
}

func validRelayRecord(p wire.RelayRecordParams) error {
	if err := relayIDError(p.RelayID); err != nil {
		return err
	}
	switch {
	case p.Role != wire.RelayRoleSource && p.Role != wire.RelayRoleTarget:
		return wire.Errorf(wire.ErrBadRequest, "bad role %q", p.Role)
	case p.Local.Harness == "":
		return wire.Errorf(wire.ErrBadRequest, "local.harness required")
	case p.Peer.Harness == "":
		return wire.Errorf(wire.ErrBadRequest, "peer.harness required")
	case p.Peer.Host == nil || p.Peer.Host.Label == "":
		return wire.Errorf(wire.ErrBadRequest, "peer.host.label required")
	}
	return nil
}

// sameConversation: a and b are the same local conversation, by session
// id or by harness and conversation id.
func sameConversation(a, b wire.RelayEnd) bool {
	return a.SessionID != "" && a.SessionID == b.SessionID ||
		a.ConversationID != "" && a.Harness == b.Harness && a.ConversationID == b.ConversationID
}

// localOf: l's local conversation is session s's.
func (l *relayLink) localOf(s *session) bool {
	return sameConversation(l.Local, wire.RelayEnd{Harness: s.s.Harness, ConversationID: s.s.ConversationID, SessionID: s.s.ID})
}

func fillEmpty(dst *string, src string) {
	if *dst == "" {
		*dst = src
	}
}

// relayRecord records one end of a relay (relay.record).
func (d *Daemon) relayRecord(p wire.RelayRecordParams) error {
	if err := validRelayRecord(p); err != nil {
		return err
	}
	p.Local.Host = nil
	host := *p.Peer.Host
	p.Peer.Host = &host
	d.mu.Lock()
	defer d.mu.Unlock()
	// A live session recorded by its id names its conversation already.
	if s := d.sessions[p.Local.SessionID]; s != nil && p.Local.ConversationID == "" && s.s.Harness == p.Local.Harness {
		p.Local.ConversationID = s.s.ConversationID
	}
	touched := map[*session]bool{}
	d.touchRelayLocked(touched, p.Local)
	i := slices.IndexFunc(d.relays, func(l relayLink) bool { return l.RelayID == p.RelayID && l.Role == p.Role })
	if i >= 0 {
		l := &d.relays[i]
		d.touchRelayLocked(touched, l.Local)
		fillEmpty(&l.Local.ConversationID, p.Local.ConversationID)
		fillEmpty(&l.Local.SessionID, p.Local.SessionID)
		fillEmpty(&l.Peer.ConversationID, p.Peer.ConversationID)
		fillEmpty(&l.Peer.SessionID, p.Peer.SessionID)
		fillEmpty(&l.Peer.Host.HostID, p.Peer.Host.HostID)
	} else {
		d.relays = append(d.relays, relayLink{
			RelayID: p.RelayID, Role: p.Role, At: d.opts.Now().UnixMilli(), Local: p.Local, Peer: p.Peer,
		})
		i = len(d.relays) - 1
	}
	d.oneLinkPerRoleLocked(d.relays[i], touched)
	if n := len(d.relays) - relaysKeep; n > 0 {
		for _, o := range d.relays[:n] {
			d.touchRelayLocked(touched, o.Local)
		}
		d.relays = slices.Delete(d.relays, 0, n)
	}
	d.saveRelaysLocked()
	d.relaysTouchedLocked(touched)
	return nil
}

// relayForget forgets every link of relay id (relay.forget).
func (d *Daemon) relayForget(id string) error {
	if err := relayIDError(id); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	touched := map[*session]bool{}
	n := len(d.relays)
	d.relays = slices.DeleteFunc(d.relays, func(l relayLink) bool {
		if l.RelayID != id {
			return false
		}
		d.touchRelayLocked(touched, l.Local)
		return true
	})
	if len(d.relays) == n {
		return nil
	}
	d.saveRelaysLocked()
	d.relaysTouchedLocked(touched)
	return nil
}

// touchRelayLocked adds the live sessions of conversation local to touched.
func (d *Daemon) touchRelayLocked(touched map[*session]bool, local wire.RelayEnd) {
	l := relayLink{Local: local}
	for _, s := range d.sessions {
		if !s.ended && l.localOf(s) {
			touched[s] = true
		}
	}
}

// relaysTouchedLocked pushes the sessions whose trace may have changed.
func (d *Daemon) relaysTouchedLocked(touched map[*session]bool) {
	for s := range touched {
		d.sessionChangedLocked(s)
	}
}

// fillRelaysLocked fills in session s's conversation id, once it has one,
// where the trace names s without it: the links recorded by s's id as
// their local conversation, and — a relay within this host, whose target
// end this daemon recorded with s — the source links naming s as their
// peer. It answers the other live sessions whose trace changed (saved
// when anything did).
func (d *Daemon) fillRelaysLocked(s *session) map[*session]bool {
	if s.s.ConversationID == "" {
		return nil
	}
	conv, h := s.s.ConversationID, s.s.Harness
	targetHere := map[string]bool{} // relays whose target end here is s
	var filled []relayLink
	for i := range d.relays {
		l := &d.relays[i]
		if l.Local.SessionID == s.s.ID && l.Local.Harness == h {
			if l.Role == wire.RelayRoleTarget {
				targetHere[l.RelayID] = true
			}
			if l.Local.ConversationID == "" {
				l.Local.ConversationID = conv
				filled = append(filled, *l)
			}
		}
	}
	touched := map[*session]bool{}
	peerFilled := false
	for i := range d.relays {
		l := &d.relays[i]
		if l.Role == wire.RelayRoleSource && targetHere[l.RelayID] && l.Peer.SessionID == s.s.ID &&
			l.Peer.Harness == h && l.Peer.ConversationID == "" {
			l.Peer.ConversationID = conv
			peerFilled = true
			d.touchRelayLocked(touched, l.Local)
		}
	}
	for _, l := range filled {
		d.oneLinkPerRoleLocked(l, touched)
	}
	if filled != nil || peerFilled {
		d.saveRelaysLocked()
	}
	delete(touched, s)
	return touched
}

// relayOfSessionLocked sets s.s.Relay, the trace of the session's
// conversation.
func (d *Daemon) relayOfSessionLocked(s *session) {
	s.s.Relay = d.relayLinksLocked(func(l *relayLink) bool { return l.localOf(s) })
}

// oneLinkPerRoleLocked keeps one link of key's conversation and role, the
// newest (the later recorded of the same at), adding the live sessions of
// those it drops to touched (nil: none).
func (d *Daemon) oneLinkPerRoleLocked(key relayLink, touched map[*session]bool) {
	same := func(l relayLink) bool { return l.Role == key.Role && sameConversation(l.Local, key.Local) }
	keep := -1
	for i, l := range d.relays {
		if same(l) && (keep < 0 || l.At >= d.relays[keep].At) {
			keep = i
		}
	}
	if keep < 0 {
		return
	}
	kept := d.relays[keep]
	d.relays = slices.DeleteFunc(d.relays, func(l relayLink) bool {
		if l.RelayID == kept.RelayID && l.Role == kept.Role || !same(l) {
			return false
		}
		if touched != nil {
			d.touchRelayLocked(touched, l.Local)
		}
		return true
	})
}

// relayOfConversationLocked is the trace of conversation conv of harness h.
func (d *Daemon) relayOfConversationLocked(h, conv string) *wire.RelayLinks {
	local := wire.RelayEnd{Harness: h, ConversationID: conv}
	return d.relayLinksLocked(func(l *relayLink) bool { return sameConversation(l.Local, local) })
}

// relayLinksLocked builds the trace from the newest link of each role that
// match takes (nil: none).
func (d *Daemon) relayLinksLocked(match func(*relayLink) bool) *wire.RelayLinks {
	var out wire.RelayLinks
	for i := range d.relays {
		l := &d.relays[i]
		if !match(l) {
			continue
		}
		dst := &out.Target
		if l.Role == wire.RelayRoleTarget {
			dst = &out.Source
		}
		if *dst != nil && (*dst).At > l.At {
			continue
		}
		link := wire.RelayLink{
			RelayID: l.RelayID, At: l.At, Harness: l.Peer.Harness,
			ConversationID: l.Peer.ConversationID, SessionID: l.Peer.SessionID,
		}
		if l.Peer.Host != nil {
			link.Host = *l.Peer.Host
		}
		*dst = &link
	}
	if out.Source == nil && out.Target == nil {
		return nil
	}
	return &out
}
