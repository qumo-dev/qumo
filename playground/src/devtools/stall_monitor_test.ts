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
	const found = stops([[0, 1], [20, 320]]);

	assertEquals(found, [undefined, 300]);
});

Deno.test("the ticks that queued up behind a stop are not stops of their own", () => {
	// The main thread is away from 21 to 320; four ticks wait for it.
	const found = stops([[0, 1], [20, 320], [40, 320], [60, 321], [80, 321], [100, 101]]);

	assertEquals(found, [undefined, 300, undefined, undefined, undefined, undefined]);
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
	const found = stops([[0, 1], [20, 21], [5020, 5021], [5040, 5041]]);

	assertEquals(found, [undefined, undefined, undefined, undefined]);
});

Deno.test("two stops close together are both found", () => {
	const found = stops([[0, 1], [20, 220], [240, 241], [260, 460]]);

	assertEquals(found, [undefined, 200, undefined, 200]);
});

Deno.test("the first tick counts if it waited", () => {
	const found = stops([[0, 150]]);

	assertEquals(found, [150]);
});
