package allowall

import (
	"context"
	"strings"
	"testing"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, err := Serve(ctx)
	require.NoError(t, err)
	client, err := auth.NewClient(url)
	require.NoError(t, err)

	g, err := client.Connect(ctx, auth.Request{ID: "1", Event: auth.EventConnect, Path: "/any"})

	require.NoError(t, err)
	assert.True(t, g.Publish.Contains(moqt.BroadcastPath("/any/where")))
	assert.True(t, g.Subscribe.Contains(moqt.BroadcastPath("/")))
	assert.True(t, strings.HasPrefix(url, "http://127.0.0.1:"))
}
