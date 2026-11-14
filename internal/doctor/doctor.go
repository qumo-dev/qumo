// Package doctor implements the read-only `qumo doctor` command: it explains
// the relay's effective runtime configuration and offers operator guidance
// without mutating anything. Its sections are the GC target and the capacity
// watermarks; the report is structured so further checks (sockets, QUIC,
// kernel) can slot in.
package doctor

import (
	"fmt"
	"io"
	"os"

	"github.com/qumo-dev/qumo/internal/gctune"
	"github.com/qumo-dev/qumo/internal/watermark"
)

// Run executes `qumo doctor`, writing the report to stdout. It is read-only and
// changes no configuration. It currently takes no arguments and rejects any, so
// a typo or stray flag gets feedback rather than a silently-ignored full report
// (a section filter like `doctor gc` is a future addition).
func Run(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("doctor takes no arguments (got %q); it reports all sections", args)
	}
	return report(os.Stdout, gctune.EnvFromOS(), capacityEnvFromOS())
}

// capacityEnv is the raw values of the capacity watermarks, as the relay
// reads them.
type capacityEnv struct {
	maxClientSessions string
	maxEgressKbps     string
}

func capacityEnvFromOS() capacityEnv {
	return capacityEnv{
		maxClientSessions: os.Getenv(watermark.EnvMaxClientSessions),
		maxEgressKbps:     os.Getenv(watermark.EnvMaxEgressKbps),
	}
}

// report renders the doctor report for env and capacity to w. It is
// separated from Run so tests can capture output and vary the environment
// without touching os.Getenv.
func report(w io.Writer, env gctune.Env, capacity capacityEnv) error {
	ew := &errWriter{w: w}
	ew.printf("qumo doctor — effective runtime configuration\n\n")
	gcSection(ew, env)
	ew.printf("\n")
	capacitySection(ew, capacity)
	return ew.err
}

// capacitySection explains the capacity watermarks: each input, where it
// makes the relay full and where it refuses, and what that does.
func capacitySection(ew *errWriter, env capacityEnv) {
	ew.printf("Capacity (when the relay is full, and when it refuses)\n")
	ew.printf("  Inputs:\n")
	ew.printf("    %s = %s\n", watermark.EnvMaxClientSessions, orUnset(env.maxClientSessions))
	ew.printf("    %s     = %s\n", watermark.EnvMaxEgressKbps, orUnset(env.maxEgressKbps))
	limitLine(ew, "Client sessions:", watermark.EnvMaxClientSessions, env.maxClientSessions, "")
	limitLine(ew, "Egress:         ", watermark.EnvMaxEgressKbps, env.maxEgressKbps, " kbit/s")
	ew.printf("  Guidance:     Full is reported as qumo_relay_full: a control plane that reads it\n")
	ew.printf("                offers the relay last. At the ceiling, 10%% above, the relay refuses\n")
	ew.printf("                new client sessions at connect (503, or a close with \"full\") and the\n")
	ew.printf("                client tries its next relay; sessions it holds and relay peers are\n")
	ew.printf("                never refused. Egress counts all the relay sends, peers included,\n")
	ew.printf("                since they share its line. Set a hub's %s below an\n", watermark.EnvMaxEgressKbps)
	ew.printf("                edge's, so that a hub busy feeding its edges sends clients to them.\n")
}

// limitLine explains one watermark, or warns that the relay will not start
// with its value.
func limitLine(ew *errWriter, label, key, value, unit string) {
	limit, err := watermark.Parse(key, value)
	switch {
	case err != nil:
		ew.printf("  Warning:      %v: the relay will not start with it\n", err)
	case limit == 0:
		ew.printf("  %s no limit\n", label)
	default:
		ew.printf("  %s full at %d%s, room again at or below %d, new client sessions refused from %d\n",
			label, limit, unit, watermark.Room(limit), watermark.Ceiling(limit))
	}
}

// gcSection explains the garbage-collector target: every input, the effective
// value, which input won and why, any warnings, and workload guidance.
func gcSection(ew *errWriter, env gctune.Env) {
	plan := gctune.Resolve(env)

	ew.printf("GC target (garbage collector)\n")
	ew.printf("  Inputs:\n")
	ew.printf("    GOGC        = %s\n", orUnset(env.GOGC))
	ew.printf("    RELAY_GOGC  = %s\n", orUnset(env.RelayGOGC))
	ew.printf("    GOMEMLIMIT  = %s\n", orUnset(env.GOMEMLIMIT))
	ew.printf("  Effective:    %s  (source: %s)\n", effectiveLabel(plan, env), plan.Source)
	ew.printf("  Why:          %s\n", plan.Reason)

	for _, warn := range plan.Warnings {
		ew.printf("  Warning:      %s\n", warn)
	}
	if env.GOMEMLIMIT != "" {
		ew.printf("  Warning:      GOMEMLIMIT is set (%s); not recommended for the relay's large stable live set — capping memory forces constant GC and collapses throughput.\n", env.GOMEMLIMIT)
	}

	ew.printf("  Guidance:     A fan-out relay's goroutine stacks dominate RSS, so GC-scan CPU\n")
	ew.printf("                grows with session count and becomes the ceiling. On big-memory\n")
	ew.printf("                hosts pushing >15K sessions, set RELAY_GOGC (600–1600 reached\n")
	ew.printf("                ~18–20K on an 8-core host). GOGC always overrides. Do not set\n")
	ew.printf("                GOMEMLIMIT for this workload.\n")
}

// errWriter wraps an io.Writer and remembers the first write error, so a run of
// best-effort Fprintf calls can be checked once at the end rather than after
// each line (the "errors are values" pattern). Once a write fails, later
// printf calls are no-ops.
type errWriter struct {
	w   io.Writer
	err error
}

// printf formats and writes one line unless a prior write already failed.
func (e *errWriter) printf(format string, a ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, a...)
}

// effectiveLabel renders the GC target in force for display. RELAY_GOGC and the
// runtime default resolve to a percentage; a GOGC that the runtime owns is
// shown as its raw value, which may be non-numeric (e.g. "off").
func effectiveLabel(plan gctune.Plan, env gctune.Env) string {
	if plan.Source == gctune.SourceGOGC {
		return fmt.Sprintf("GOGC=%s (runtime-controlled)", env.GOGC)
	}
	return fmt.Sprintf("%d%%", plan.EffectivePercent())
}

// orUnset renders an empty env value as an explicit "(unset)" marker.
func orUnset(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}
