package usage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRecord_Validate(t *testing.T) {
	bytes := map[string]int64{IngressBytes: 10, EgressBytes: 20}
	ts := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		record  Record
		wantErr bool
	}{
		"an open":             {record: Record{Type: SessionOpen, SessionID: "s1", KID: "k1", TS: ts}},
		"usage":               {record: Record{Type: Usage, SessionID: "s1", KID: "k1", Metrics: bytes, TS: ts}},
		"a close":             {record: Record{Type: SessionClose, SessionID: "s1", KID: "k1", Metrics: bytes, Reason: ReasonClosed, TS: ts}},
		"a viewers total":     {record: Record{Type: Viewers, Run: "r1", KID: "k1", Metrics: bytes, TS: ts}},
		"an unknown type":     {record: Record{Type: "both", SessionID: "s1", KID: "k1", TS: ts}, wantErr: true},
		"no type":             {record: Record{SessionID: "s1", KID: "k1", TS: ts}, wantErr: true},
		"no kid":              {record: Record{Type: SessionOpen, SessionID: "s1", TS: ts}, wantErr: true},
		"no ts":               {record: Record{Type: SessionOpen, SessionID: "s1", KID: "k1"}, wantErr: true},
		"no session id":       {record: Record{Type: Usage, KID: "k1", Metrics: bytes, TS: ts}, wantErr: true},
		"a session's run":     {record: Record{Type: Usage, SessionID: "s1", Run: "r1", KID: "k1", Metrics: bytes, TS: ts}, wantErr: true},
		"viewers, no run":     {record: Record{Type: Viewers, KID: "k1", Metrics: bytes, TS: ts}, wantErr: true},
		"viewers' session":    {record: Record{Type: Viewers, Run: "r1", SessionID: "s1", KID: "k1", Metrics: bytes, TS: ts}, wantErr: true},
		"usage, no metrics":   {record: Record{Type: Usage, SessionID: "s1", KID: "k1", TS: ts}, wantErr: true},
		"a close, no metrics": {record: Record{Type: SessionClose, SessionID: "s1", KID: "k1", TS: ts}, wantErr: true},
		"viewers, no metrics": {record: Record{Type: Viewers, Run: "r1", KID: "k1", TS: ts}, wantErr: true},
		"a negative metric": {
			record:  Record{Type: Usage, SessionID: "s1", KID: "k1", Metrics: map[string]int64{EgressBytes: -1}, TS: ts},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.record.Validate()

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}
