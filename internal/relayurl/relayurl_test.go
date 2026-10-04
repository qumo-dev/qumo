package relayurl

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedact(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want string
	}{
		"native QUIC with a credential":  {raw: "moqt://relay.example.com:4433/acme?jwt=h.p.s", want: "moqt://relay.example.com:4433/acme"},
		"WebTransport with a credential": {raw: "https://relay.example.com/acme?jwt=h.p.s&x=1", want: "https://relay.example.com/acme"},
		"user info and fragment":         {raw: "https://user@relay.example.com/acme#frag", want: "https://relay.example.com/acme"},
		"no query":                       {raw: "moqt://127.0.0.1:4433", want: "moqt://127.0.0.1:4433"},
		"unparsable":                     {raw: "moqt://[bad/acme?jwt=h.p.s", want: "moqt://[bad/acme"},
		"empty":                          {raw: "", want: ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, Redact(tt.raw))
		})
	}
}

func TestScrubError(t *testing.T) {
	const raw = "https://relay.example.com/acme?jwt=h.p.s"
	cause := errors.New(`dial "` + raw + `": refused`)
	tests := map[string]struct {
		err      error
		raw      string
		wantText string
	}{
		"an error quoting the URL": {err: cause, raw: raw, wantText: `dial "https://relay.example.com/acme": refused`},
		"no query in the URL":      {err: cause, raw: "moqt://relay:4433", wantText: cause.Error()},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := ScrubError(tt.err, tt.raw)

			assert.Equal(t, tt.wantText, got.Error())
			assert.ErrorIs(t, got, cause, "the original error stays in the chain")
		})
	}
	t.Run("nil", func(t *testing.T) {
		assert.NoError(t, ScrubError(nil, raw))
	})
}
