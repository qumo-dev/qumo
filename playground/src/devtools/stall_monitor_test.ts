// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { StallDetector } from "./stall_monitor.ts";

// One tick: when it was sent and when it was handled.
type Tick = readonly [sentAt: number, handledAt: number];

// What the detector says of each tick in turn. The ticks are sent 20 ms apart.
function stops(ticks: readonly Tick[]): (number | undefined)[] {
	const detector = new StallDetector(20);
	return ticks.map(([sentAt, handledAt]) => detector.handled(sentAt, handledAt));
}

Deno.test("ticks handled as they are sent show no stop", () => {
	const found = stops([[0, 1], [20, 21], [40, 42], [60, 60]]);

	assertEquals(found, [undefined, undefined, undefined, undefined]);
});

Deno.test("a stop is measured from the middle of the stretch in which it began", () => {
	// Free when the tick at 0 was handled, busy when the one at 20 was sent,
	// until 320: the stop began somewhere in those 20 ms, and 10 is the guess.
	const found = stops([[0, 0], [20, 320]]);

	assertEquals(found, [undefined, 310]);
});

Deno.test("the ticks that queued up behind a stop are not stops of their own", () => {
	// The main thread is away until 320; four more ticks wait for it.
	const found = stops([[0, 0], [20, 320], [40, 320], [60, 321], [80, 321], [100, 100]]);

	assertEquals(found, [undefined, 310, undefined, undefined, undefined, undefined]);
});

Deno.test("more work between the queued ticks is counted once, as its own stop", () => {
	// Stopped until 320; then, before the next queued tick is handled, the
	// page is busy for another 100 ms.
	const found = stops([[0, 0], [20, 320], [40, 420], [60, 420], [440, 440]]);

	// 310 and then 100: not 310 and then the 380 the second tick had waited.
	assertEquals(found, [undefined, 310, 100, undefined, undefined]);
});

Deno.test("a wait under 30 ms is not a stop", () => {
	const cases = [
		{ name: "just under", waited: 29, want: undefined },
		// Over the bar, and so reported from the middle of the unseen stretch.
		{ name: "exactly", waited: 30, want: 40 },
	] as const;

	for (const c of cases) {
		assertEquals(stops([[0, 0], [20, 20 + c.waited]])[1], c.want, c.name);
	}
});

Deno.test("a ticker that paused, as in a frozen tab, shows no stop", () => {
	// Nothing is sent for five seconds; what is sent is handled at once.
	const found = stops([[0, 0], [20, 20], [5020, 5020], [5040, 5040]]);

	assertEquals(found, [undefined, undefined, undefined, undefined]);
});

Deno.test("a stop after the ticker paused is measured from when its tick was sent", () => {
	// Five seconds of nothing sent, then a tick that waits 200 ms: nothing is
	// known of the silence, so none of it is added.
	const found = stops([[0, 0], [5000, 5200]]);

	assertEquals(found, [undefined, 200]);
});

Deno.test("a tick sent late is still counted from the middle of what went unseen", () => {
	// The ticker skipped a beat: free at 0, the next tick not sent until 60,
	// and that one waits until 300. The stop began somewhere in those 60 ms.
	const found = stops([[0, 0], [60, 300]]);

	assertEquals(found, [undefined, 270]);
});

Deno.test("two stops close together are both found", () => {
	const found = stops([[0, 0], [20, 220], [240, 240], [260, 460]]);

	assertEquals(found, [undefined, 210, undefined, 210]);
});

Deno.test("clocks that disagree by a fixed amount change nothing", () => {
	const cases = [
		{ name: "the sender's ahead by a minute", skew: 60_000 },
		{ name: "the sender's behind by a minute", skew: -60_000 },
	] as const;

	for (const c of cases) {
		// The same ticks as a stop until 340, with every send time shifted.
		const found = stops([[0 + c.skew, 0], [20 + c.skew, 20], [40 + c.skew, 340]]);

		assertEquals(found, [undefined, undefined, 310], c.name);
	}
});

Deno.test("the first tick shows no stop: there is nothing yet to measure it against", () => {
	const found = stops([[0, 150]]);

	assertEquals(found, [undefined]);
});
