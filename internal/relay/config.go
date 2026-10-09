package relay

import (
	"strings"
)

// splitAddrList splits PEERS, a comma-separated list of host:port entries,
// trimming whitespace and dropping empty entries.
func splitAddrList(raw string) []string {
	var addrs []string
	for a := range strings.SplitSeq(raw, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

// Config holds the relay server configuration.
type Config struct {
	// NodeID is a human-readable identifier for this relay node, used as a
	// prefix in routing/metric labels (e.g. "[node]/path/track"). It is the
	// ops-facing label, not the MoQT protocol hop identity (that is the
	// TrackMux's random HopID — see moqt.NewHopID).
	NodeID string

	// Role is this node's topology role: "hub" (inter-region), "edge"
	// (client-facing), or empty for a flat / single-node relay. Set via the
	// `qumo relay --role` flag (execution mode); there is no env equivalent.
	Role string

	// GroupCacheSize is the number of completed groups each track's ring
	// retains for late/backfill subscribers. ≤0 falls back to
	// DefaultGroupCacheSize.
	GroupCacheSize int

	// FrameCapacity is the capacity (bytes) of recycled frame buffers. ≤0
	// falls back to DefaultFramePool (DefaultNewFrameCapacity).
	FrameCapacity int

	// Peers is the list of relays to dial. Each host is resolved to all its
	// addresses, and each address is dialed, so a DNS name with several
	// records (e.g. role-hub.qumo-relay.service.consul:4433) connects to
	// every relay behind it. The relay discovers their announcements via
	// ANNOUNCE_PLEASE and registers them on the local TrackMux.
	Peers []Peer

	// WebSocket makes HandleWebTransport also take WebSocket upgrades and
	// serve them as MoQ sessions over QMux, for clients whose WebTransport
	// does not work (every browser on WebKit). Such a session is admitted
	// exactly as a WebTransport one. Set via WS_ENABLE or WS_TLS_ADDR.
	WebSocket bool

	// NextSessionURI is the redirect URI sent to clients/peers in a GOAWAY
	// message during graceful shutdown (gomoqt Server.NextSessionURI). Empty
	// means no redirect is advertised. GOAWAY is an escape-hatch primitive;
	// route/subscription migration is the primary mobility mechanism (#280).
	NextSessionURI string
}

// Peer represents a remote relay to connect to for announce discovery.
type Peer struct {
	// Address is the remote relay's host:port, such as "relay-tokyo:4433",
	// dialed over native QUIC (moqt://).
	Address string
}
