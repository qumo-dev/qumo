// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals, assertStrictEquals, assertThrows } from "@std/assert";
import { AudioRing } from "./ring.ts";

// At 1000 samples a second one sample is one millisecond, which keeps the
// arithmetic in these tests readable.
const RATE = 1000;
const MS = 1000; // timestamps are microseconds

// A ring that holds twice its latency, so the tests can fill it with a few samples.
function ring(latency: number, channels = 1): AudioRing {
	return new AudioRing({ rate: RATE, channels, latency, headroom: latency });
}

// A block of `length` samples counting up from `from`.
function block(from: number, length: number): Float32Array[] {
	return [Float32Array.from({ length }, (_, i) => from + i)];
}

// Reads `length` samples of the first channel.
function read(r: AudioRing, length: number): number[] {
	const out = [new Float32Array(length)];
	r.read(out);
	return Array.from(out[0] ?? []);
}

Deno.test("holds playback back until the latency is buffered", () => {
	const r = ring(4);
	r.write(0, block(1, 3));

	const before = read(r, 2);
	r.write(3 * MS, block(4, 1));
	const after = read(r, 2);

	assertEquals(before, [0, 0]);
	assertEquals(after, [1, 2]);
});

Deno.test("drops a block older than where playback started", () => {
	const r = ring(2);
	r.write(2 * MS, block(3, 2));

	r.write(0, block(1, 2));

	assertEquals(read(r, 2), [3, 4]);
});

Deno.test("places an out-of-order block that is still ahead of playback", () => {
	const r = ring(4);
	r.write(0, block(1, 2));
	r.write(4 * MS, block(5, 2));
	r.write(2 * MS, block(3, 2));

	assertEquals(read(r, 6), [1, 2, 3, 4, 5, 6]);
});

Deno.test("leaves silence the length of a block that never arrives", () => {
	const r = ring(4);
	r.write(0, block(1, 2));
	r.write(4 * MS, block(5, 2));

	assertEquals(read(r, 6), [1, 2, 0, 0, 5, 6]);
});

Deno.test("joins a block whose timestamp is a sample off the end of the last", () => {
	const cases = [
		{ name: "a sample late", at: 3 },
		{ name: "a sample early", at: 1 },
	];

	for (const c of cases) {
		const r = ring(4);
		r.write(0, block(1, 2));

		r.write(c.at * MS, block(3, 2));

		assertEquals(read(r, 4), [1, 2, 3, 4], c.name);
	}
});

Deno.test("drops the part of a block that playback has already passed", () => {
	const r = ring(2);
	r.write(0, block(1, 4));
	read(r, 3);

	r.write(1 * MS, block(20, 4)); // covers positions 1..4; 1 and 2 are played

	assertEquals(read(r, 2), [22, 23]);
});

Deno.test("running dry holds playback back until the buffer refills", () => {
	const r = ring(4);
	r.write(0, block(1, 4));
	const first = read(r, 6); // two more than is buffered

	r.write(6 * MS, block(7, 2));
	const starved = read(r, 2);
	r.write(8 * MS, block(9, 2));
	const refilled = read(r, 4);

	assertEquals(first, [1, 2, 3, 4, 0, 0]);
	assertEquals(starved, [0, 0]);
	assertEquals(refilled, [7, 8, 9, 10]);
	assertStrictEquals(r.underruns, 1);
});

Deno.test("is stalled exactly while it is holding playback back", () => {
	const r = ring(4);
	const atStart = r.stalled;
	r.write(0, block(1, 4));
	const whenFull = r.stalled;
	read(r, 5);

	assertEquals([atStart, whenFull, r.stalled], [true, false, true]);
});

Deno.test("catches up to the latency when more arrives than it can hold", () => {
	const r = ring(2); // holds 4 samples
	r.write(0, block(1, 4));

	r.write(4 * MS, block(5, 2));

	// Not [3, 4, 5, 6]: that would leave the ring full, spilling on every burst.
	assertEquals(read(r, 2), [5, 6]);
	assertStrictEquals(r.buffered, 0);
});

Deno.test("repeats the first channel for a block with fewer channels than it plays", () => {
	const r = ring(2, 2);
	r.write(0, block(1, 2));
	const out = [new Float32Array(2), new Float32Array(2)];

	r.read(out);

	assertEquals(out.map((channel) => Array.from(channel)), [[1, 2], [1, 2]]);
});

Deno.test("follows a timeline that restarts far behind playback", () => {
	const r = ring(2);
	r.write(1_000_000 * MS, block(1, 2));
	read(r, 2);

	r.write(0, block(7, 2));

	assertEquals(read(r, 2), [7, 8]);
});

Deno.test("raising the latency holds playback back until the buffer covers it", () => {
	const r = ring(2);
	r.write(0, block(1, 2));

	r.resize(4);
	const held = read(r, 2);
	r.write(2 * MS, block(3, 2));
	const resumed = read(r, 4);

	assertEquals(held, [0, 0]);
	assertEquals(resumed, [1, 2, 3, 4]);
});

Deno.test("lowering the latency keeps the newest audio that still fits", () => {
	const r = ring(4); // holds 8: the latency and 4 of headroom
	r.write(0, block(1, 8));

	r.resize(2); // holds 6

	assertEquals(read(r, 6), [3, 4, 5, 6, 7, 8]);
});

Deno.test("counts what it had to do", () => {
	const r = ring(4); // holds 8 samples
	r.write(0, block(1, 4));
	read(r, 6); // four samples of audio, then two of silence: it ran dry
	r.write(0, block(9, 2)); // both samples are already played
	r.write(6 * MS, block(9, 2)); // two samples on from the last block: a gap
	r.write(8 * MS, block(9, 6)); // too much: four buffered samples and two of its own go

	const { underruns, starved, late, gaps, overflowed } = r.stats;

	assertEquals({ underruns, starved, late, gaps, overflowed }, {
		underruns: 1,
		starved: 2,
		late: 2,
		gaps: 2,
		overflowed: 6,
	});
});

Deno.test("silence before any audio has arrived is not counted as starvation", () => {
	const r = ring(2);

	read(r, 8);

	assertStrictEquals(r.stats.starved, 0);
});

Deno.test("skips audio it has held for two seconds without ever needing", () => {
	const r = new AudioRing({ rate: RATE, channels: 1, latency: 4, headroom: 100 });
	r.write(0, block(0, 20)); // 16 more than the latency

	// Two seconds of steady playback: ten samples in, ten out.
	for (let at = 20; at < 2020; at += 10) {
		r.write(at * MS, block(at, 10));
		read(r, 10);
	}

	assertStrictEquals(r.stats.trimmed, 16);
	assertStrictEquals(r.buffered, 4);
});

Deno.test("keeps audio above the latency that playback does dip into", () => {
	const r = new AudioRing({ rate: RATE, channels: 1, latency: 10, headroom: 100 });
	r.write(0, block(0, 30));

	// Bursts: thirty samples arrive, then playback draws the buffer down to ten.
	for (let at = 30; at < 3030; at += 30) {
		read(r, 20);
		r.write(at * MS, block(at, 30));
		read(r, 10);
	}

	assertStrictEquals(r.stats.trimmed, 0);
});

Deno.test("by default leaves at least 200 ms of room above the latency", () => {
	const r = new AudioRing({ rate: RATE, channels: 1, latency: 50 });
	r.write(0, block(1, 250));
	read(r, 10);

	r.write(250 * MS, block(251, 11)); // one sample more than 50 + 200

	assertStrictEquals(r.stats.overflowed, 201);
});

Deno.test("a backlog passed over before anything has played is not counted as lost", () => {
	const r = ring(4); // holds 8 samples

	r.write(0, block(1, 20));

	assertStrictEquals(r.stats.overflowed, 0);
});

Deno.test("starts at the newest audio when it begins with a backlog", () => {
	const r = ring(4); // holds 8 samples

	r.write(0, block(1, 20));

	assertEquals(read(r, 4), [17, 18, 19, 20]);
});

Deno.test("a backlog after a reset is not counted as lost either", () => {
	const r = ring(4);
	r.write(0, block(1, 4));
	read(r, 2);

	r.reset();
	r.write(100 * MS, block(1, 20));

	assertStrictEquals(r.stats.overflowed, 0);
});

Deno.test("takeLow gives the bottom of the swing since it was last asked", () => {
	const r = ring(4);
	r.write(0, block(0, 8));
	read(r, 6); // down to 2
	r.write(8 * MS, block(8, 5)); // back up to 7

	const first = r.takeLow();
	read(r, 1); // down to 6

	assertStrictEquals(first, 2);
	assertStrictEquals(r.takeLow(), 6);
});

Deno.test("after a reset it starts again from whatever arrives next", () => {
	const r = ring(2);
	r.write(1_000_000 * MS, block(1, 4));
	read(r, 1);

	r.reset();
	const empty = read(r, 2);
	r.write(5 * MS, block(7, 2));

	assertEquals(empty, [0, 0]);
	assertEquals(read(r, 2), [7, 8]);
});

Deno.test("rejects a latency that is not positive", () => {
	assertThrows(() => ring(0), RangeError);
});
