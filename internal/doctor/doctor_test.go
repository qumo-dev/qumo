package doctor

import (
	"bytes"
	"testing"

	"github.com/qumo-dev/qumo/internal/gctune"
	"github.com/stretchr/testify/assert"
)

func TestReport(t *testing.T) {
	tests := map[string]struct {
		env         gctune.Env
		wantContain []string
	}{
		"nothing set → runtime default, opt-in hint": {
			env:         gctune.Env{},
			wantContain: []string{"GOGC        = (unset)", "RELAY_GOGC  = (unset)", "100%", "source: runtime default", "Set RELAY_GOGC"},
		},
		"RELAY_GOGC applied → shows value and source": {
			env:         gctune.Env{RelayGOGC: "800"},
			wantContain: []string{"RELAY_GOGC  = 800", "800%", "source: RELAY_GOGC"},
		},
		"GOGC owns it → runtime-controlled, shadowed RELAY_GOGC surfaced": {
			env:         gctune.Env{GOGC: "150", RelayGOGC: "800"},
			wantContain: []string{"GOGC=150 (runtime-controlled)", "source: GOGC", "never overrides", "RELAY_GOGC \"800\" ignored because GOGC \"150\" is set"},
		},
		"invalid RELAY_GOGC → warning + falls back to default": {
			env:         gctune.Env{RelayGOGC: "notanint"},
			wantContain: []string{"Warning:", "invalid RELAY_GOGC", "100%", "source: runtime default"},
		},
		"GOMEMLIMIT set → discouraged warning": {
			env:         gctune.Env{RelayGOGC: "800", GOMEMLIMIT: "8GiB"},
			wantContain: []string{"GOMEMLIMIT  = 8GiB", "GOMEMLIMIT is set (8GiB); not recommended"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			report(&buf, tt.env, capacityEnv{})
			out := buf.String()
			for _, want := range tt.wantContain {
				assert.Contains(t, out, want)
			}
		})
	}
}

func TestReport_Capacity(t *testing.T) {
	tests := map[string]struct {
		env         capacityEnv
		wantContain []string
	}{
		"nothing set → no limits": {
			env:         capacityEnv{},
			wantContain: []string{"MAX_CLIENT_SESSIONS = (unset)", "MAX_EGRESS_KBPS     = (unset)", "Client sessions: no limit", "Egress:          no limit"},
		},
		"both set → full, room and ceiling": {
			env: capacityEnv{maxClientSessions: "500", maxEgressKbps: "80000"},
			wantContain: []string{
				"Client sessions: full at 500, room again at or below 450, new client sessions refused from 550",
				"Egress:          full at 80000 kbit/s, room again at or below 72000, new client sessions refused from 88000",
			},
		},
		"both invalid → both warned": {
			env:         capacityEnv{maxClientSessions: "-3", maxEgressKbps: "fast"},
			wantContain: []string{`invalid MAX_CLIENT_SESSIONS "-3"`, `invalid MAX_EGRESS_KBPS "fast"`},
		},
		"a limit of one → room at 0, refused from 2": {
			env:         capacityEnv{maxClientSessions: "1"},
			wantContain: []string{"Client sessions: full at 1, room again at or below 0, new client sessions refused from 2", "Egress:          no limit"},
		},
		"invalid → the relay will not start": {
			env:         capacityEnv{maxEgressKbps: "80M"},
			wantContain: []string{"Warning:", `invalid MAX_EGRESS_KBPS "80M"`, "the relay will not start"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer

			err := report(&buf, gctune.Env{}, tt.env)

			assert.NoError(t, err)
			out := buf.String()
			for _, want := range tt.wantContain {
				assert.Contains(t, out, want)
			}
		})
	}
}

func TestRun_RejectsArgs(t *testing.T) {
	// A stray topic/flag must get feedback, not a silently-ignored full report.
	assert.Error(t, Run([]string{"gc"}))
}
