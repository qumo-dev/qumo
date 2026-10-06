// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { StallDetector } from "./stall_monitor.ts";

// One tick: when it was sent and when it was handled.
type Tick = readonly [sentAt: number, handledAt: number];

// What the detector says of each tick in turn.
function stops(ticks: readonly Tick[]): (number | undefined)[] {
	const detector = new StallDetector();
	return ticks.map(([sentAt, handledAt]) => detector.handled(sentAt, handledAt));
}

Deno.test("ticks handled as they are sent show no stop", () => {
	const found = stops([[0, 1], [20, 21], [40, 42], [60, 60]]);

	assertEquals(found, [undefined, undefined, undefined, undefined]);
});

Deno.test("a tick handled long after it was sent is a stop of that length", () => {
	const found = stops([[0, 0], [20, 320]]);

	assertEquals(found, [undefined, 300]);
});

Deno.test("the ticks that queued up behind a stop are not stops of their own", () => {
	// The main thread is away from 20 to 320; four ticks wait for it.
	const found = stops([[0, 0], [20, 320], [40, 320], [60, 321], [80, 321], [100, 100]]);

	assertEquals(found, [undefined, 300, undefined, undefined, undefined, undefined]);
});

Deno.test("more work between the queued ticks is counted once, as its own stop", () => {
	// Stopped from 20 to 320; then, before the next queued tick is handled,
	// the page is busy for another 100 ms.
	const found = stops([[0, 0], [20, 320], [40, 420], [60, 420], [440, 440]]);

	// 300 and then 100: not 300 and then the 380 the second tick had waited.
	assertEquals(found, [undefined, 300, 100, undefined, undefined]);
});

Deno.test("a wait under 30 ms is not a stop", () => {
	const cases = [
		{ name: "just under", waited: 29, want: undefined },
		{ name: "exactly", waited: 30, want: 30 },
	] as const;

	for (const c of cases) {
		assertEquals(stops([[0, 0], [100, 100 + c.waited]])[1], c.want, c.name);
	}
});

Deno.test("a ticker that paused, as in a frozen tab, shows no stop", () => {
	// Nothing is sent for five seconds; what is sent is handled at once.
	const found = stops([[0, 0], [20, 20], [5020, 5020], [5040, 5040]]);

	assertEquals(found, [undefined, undefined, undefined, undefined]);
});

Deno.test("two stops close together are both found", () => {
	const found = stops([[0, 0], [20, 220], [240, 240], [260, 460]]);

	assertEquals(found, [undefined, 200, undefined, 200]);
});

Deno.test("clocks that disagree by a fixed amount change nothing", () => {
	const cases = [
		{ name: "the sender's ahead by a minute", skew: 60_000 },
		{ name: "the sender's behind by a minute", skew: -60_000 },
	] as const;

	for (const c of cases) {
		// The same ticks as a 300 ms stop, with every send time shifted.
		const found = stops([[0 + c.skew, 0], [20 + c.skew, 20], [40 + c.skew, 340]]);

		assertEquals(found, [undefined, undefined, 300], c.name);
	}
});

Deno.test("the first tick shows no stop: there is nothing yet to measure it against", () => {
	const found = stops([[0, 150]]);

	assertEquals(found, [undefined]);
});
