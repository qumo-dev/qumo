// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { InputGaps } from "./gaps.ts";

// Feeds chunks with the given input timestamps to a decoder that, like the
// browser's, stamps its output by counting on from its first input in steps of
// `step`, and returns the corrected output timestamps.
function corrected(inputs: number[], step: number): number[] {
	const gaps = new InputGaps();
	for (const timestamp of inputs) gaps.fed(timestamp);
	return inputs.map((_, i) => gaps.decoded((inputs[0] ?? 0) + i * step, step));
}

Deno.test("leaves the timestamps of an unbroken stream alone", () => {
	assertEquals(corrected([0, 20, 40, 60], 20), [0, 20, 40, 60]);
});

Deno.test("puts back the time of a frame that was never fed", () => {
	// The frame at 60 is missing: the decoder stamps 80 as 60, 100 as 80.
	assertEquals(corrected([0, 20, 40, 80, 100], 20), [0, 20, 40, 80, 100]);
});

Deno.test("adds up several gaps", () => {
	assertEquals(corrected([0, 20, 60, 80, 140], 20), [0, 20, 60, 80, 140]);
});

Deno.test("does not take rounded timestamps for gaps", () => {
	// 21.33 ms frames with timestamps rounded to the millisecond.
	assertEquals(corrected([0, 21, 43, 64, 85], 21), [0, 21, 42, 63, 84]);
});

Deno.test("is back in step right after one irregular timestamp", () => {
	// The odd one is followed as given; nothing after it is moved.
	assertEquals(corrected([0, 20, 21, 60, 80], 20), [0, 20, 21, 60, 80]);
});

Deno.test("follows a timeline that jumps back", () => {
	assertEquals(corrected([1000, 1020, 0, 20], 20), [1000, 1020, 0, 20]);
});

Deno.test("corrects a block by what was known when its chunk was fed", () => {
	const gaps = new InputGaps();
	gaps.fed(0);
	gaps.fed(20);
	const first = gaps.decoded(0, 20);
	gaps.fed(60); // a gap, seen after the first block came out
	const second = gaps.decoded(20, 20);
	const third = gaps.decoded(40, 20);

	assertEquals([first, second, third], [0, 20, 60]);
});

Deno.test("starts afresh after a reset", () => {
	const gaps = new InputGaps();
	gaps.fed(0);
	gaps.fed(20);
	gaps.fed(100);

	gaps.reset();
	gaps.fed(500);

	assertEquals(gaps.decoded(500, 20), 500);
});
