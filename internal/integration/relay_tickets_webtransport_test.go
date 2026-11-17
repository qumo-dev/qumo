//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/relay"
	"github.com/qumo-dev/qumo/token"
	"github.com/stretchr/testify/require"
)

// A WebTransport session that holds a ticket ends, and the relay shuts down
// promptly. The relay's Auth Stream reader got WT_SESSION_GONE from the
// closing client and waited for the session to close it, while the session's
// close waited for the reader: webtransport-go's ReceiveStream.CancelRead
// did not end a Read waiting that way.
func TestRelay_TicketOverWebTransportShutdown(t *testing.T) {
	t.Skip("needs okdaichi/webtransport-go with ReceiveStream.CancelRead ending a Read waiting after WT_SESSION_GONE; until then the relay deadlocks")

	keys, verifier := trustedKeys(t, "acme", 1)
	addr, srv := startAuthRelay(t, verifier.Authorize, nil, func(s *relay.Server) { s.Tickets = verifier })
	sign := func(g token.Grant) string {
		tok, err := token.Sign(keys[0], g, time.Minute)
		require.NoError(t, err)
		return tok
	}
	sess := dialOver(t, "https://"+addr+"/?jwt="+sign(token.Grant{}), nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := sess.Auth(ctx, []byte(sign(token.Grant{Root: "/acme/room/42/", Subscribe: []string{"/"}})))
	require.NoError(t, err)

	require.NoError(t, sess.CloseWithError(moqt.NoError, "done"))

	// moqt.Server.Shutdown does not return when its context ends, so the
	// wait is bounded here.
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("relay Shutdown did not return after the ticket-holding WebTransport session closed")
	}
}
