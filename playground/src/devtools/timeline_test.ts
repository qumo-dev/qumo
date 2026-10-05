// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import type { AudioBufferSample, GroupRecord, GroupState } from "./recorder.ts";
import {
	audioGlitches,
	audioLane,
	clockRates,
	columns,
	type Lane,
	packLanes,
	shapeAt,
	stallBands,
	trackLane,
	withBands,
} from "./timeline.ts";

function group(
	sequence: number,
	arrived: number,
	ended: number | undefined,
	rest: Partial<GroupRecord> = {},
): GroupRecord {
	const state: GroupState = ended === undefined ? "receiving" : "complete";
	return {
		sequence,
		arrived,
		ended,
		state,
		frames: 0,
		bytes: 0,
		rendered: 0,
		renderStart: undefined,
		renderEnd: undefined,
		...rest,
	};
}

// The lane each group landed on, as "sequence:lane".
function lanes(groups: GroupRecord[], now = 10_000): string[] {
	return packLanes(groups, now).map((bar) => `${bar.group.sequence}:${bar.lane}`);
}

Deno.test("packLanes", async (t) => {
	const cases = [
		{
			name: "puts groups that follow one another on one lane",
			groups: [group(0, 0, 2000), group(1, 2000, 4000), group(2, 4000, 6000)],
			want: ["0:0", "1:0", "2:0"],
		},
		{
			name: "opens a second lane for a group that overlaps",
			groups: [group(0, 0, 3000), group(1, 2000, 4000)],
			want: ["0:0", "1:1"],
		},
		{
			name: "reuses a lane once it is free",
			groups: [group(0, 0, 3000), group(1, 2000, 4000), group(2, 3500, 5000)],
			want: ["0:0", "1:1", "2:0"],
		},
		{
			name: "treats a group still being received as running to now",
			groups: [group(0, 0, undefined), group(1, 2000, 4000)],
			want: ["0:0", "1:1"],
		},
		{
			name: "orders by arrival, not by sequence",
			groups: [group(1, 2000, 4000), group(0, 0, 2000)],
			want: ["0:0", "1:0"],
		},
		{
			name: "keeps one lane when a group arrives just before the previous one ends",
			groups: [group(0, 0, 2030), group(1, 2000, 4000)],
			want: ["0:0", "1:0"],
		},
		{
			name: "opens a second lane once the overlap is longer than a hand-over",
			groups: [group(0, 0, 2051), group(1, 2000, 4000)],
			want: ["0:0", "1:1"],
		},
		{ name: "returns nothing for no groups", groups: [], want: [] },
	];

	for (const c of cases) {
		await t.step(c.name, () => {
			assertEquals(lanes(c.groups), c.want);
		});
	}
});

Deno.test("a group still being received ends at now", () => {
	const [bar] = packLanes([group(0, 1000, undefined)], 5000);

	assertEquals([bar?.start, bar?.end], [1000, 5000]);
});

Deno.test("columns counts groups by the second they arrived in", () => {
	const groups = [
		group(0, 100, 120, { frames: 1, rendered: 1 }),
		group(1, 900, 920, { frames: 1, rendered: 0, state: "skipped" }),
		group(2, 1500, 1520, { frames: 1, rendered: 1 }),
	];

	const out = columns(groups, 0, 3000, 1000);

	assertEquals(
		out.map((c) => [c.start, c.states.complete, c.states.skipped, c.frames, c.rendered]),
		[
			[0, 1, 1, 2, 1],
			[1000, 1, 0, 1, 1],
			[2000, 0, 0, 0, 0],
		],
	);
});

Deno.test("columns leaves out groups that arrived outside the range", () => {
	const out = columns([group(0, -500, -400), group(1, 3500, 3600)], 0, 3000, 1000);

	assertEquals(out.map((c) => c.states.complete), [0, 0, 0]);
});

function sample(at: number, counters: Partial<AudioBufferSample> = {}): AudioBufferSample {
	return {
		at,
		buffered: 100,
		low: 100,
		latency: 100,
		stalled: false,
		underruns: 0,
		starved: 0,
		gaps: 0,
		late: 0,
		overflowed: 0,
		trimmed: 0,
		written: 0,
		played: 0,
		...counters,
	};
}

Deno.test("audioGlitches finds nothing while the counters stand still", () => {
	const history = [sample(0, { gaps: 40 }), sample(100, { gaps: 40 }), sample(200, { gaps: 40 })];

	assertEquals(audioGlitches(history), []);
});

Deno.test("audioGlitches reports how much each counter rose, and when", () => {
	const history = [
		sample(0),
		sample(100, { starved: 20 }),
		sample(200, { starved: 20, overflowed: 104, trimmed: 12 }),
	];

	const glitches = audioGlitches(history);

	assertEquals(glitches, [
		{ at: 100, starved: 20, gaps: 0, late: 0, overflowed: 0, trimmed: 0 },
		{ at: 200, starved: 0, gaps: 0, late: 0, overflowed: 104, trimmed: 12 },
	]);
});

Deno.test("audioGlitches does not take counters starting over for a glitch", () => {
	const history = [sample(0, { overflowed: 500 }), sample(100, { overflowed: 0 })];

	assertEquals(audioGlitches(history), []);
});

Deno.test("clockRates compares how fast media arrived with how fast it played", () => {
	const history = [
		sample(0, { written: 1000, played: 0 }),
		sample(10_000, { written: 11_450, played: 10_000 }),
	];

	assertEquals(clockRates(history), { media: 1.045, output: 1 });
});

Deno.test("clockRates waits for five seconds of history", () => {
	const history = [sample(0), sample(4999, { written: 4999, played: 4999 })];

	assertEquals(clockRates(history), undefined);
});

Deno.test("clockRates gives no answer across an output restart", () => {
	const history = [sample(0, { played: 9000 }), sample(6000, { played: 100 })];

	assertEquals(clockRates(history), undefined);
});

// A lane's shapes as "style@x0-x1/y" strings, with x as percentages.
function drawn(lane: Lane): string[] {
	return lane.shapes.map((s) =>
		`${s.style}@${Math.round(s.x0 * 100)}-${Math.round(s.x1 * 100)}/${s.y}`
	);
}

Deno.test("trackLane draws a bar per group and a strip of what was rendered", () => {
	const groups = [
		group(0, 0, 2000, { rendered: 60, frames: 60, renderStart: 100, renderEnd: 2100 }),
		group(1, 2000, undefined, { frames: 30 }),
	];

	const lane = trackLane(groups, true, 4000, 4000);

	assertEquals(drawn(lane), [
		"track@0-100/15",
		"complete@0-50/0",
		"rendered@3-53/15",
		"receiving@50-100/0",
	]);
	assertEquals(lane.height, 21);
});

Deno.test("trackLane puts overlapping groups on separate rows", () => {
	const lane = trackLane([group(0, 0, 3000), group(1, 1000, 4000)], false, 4000, 4000);

	assertEquals(drawn(lane), ["complete@0-75/0", "complete@25-100/12"]);
	assertEquals(lane.height, 22);
});

Deno.test("trackLane marks a group that ended with nothing rendered", () => {
	const lane = trackLane([group(0, 1000, 1500, { state: "skipped" })], true, 4000, 4000);

	assertEquals(drawn(lane).at(-1), "unrendered@25-25/15");
});

Deno.test("trackLane gives a sent track no rendered strip", () => {
	const lane = trackLane([group(0, 0, 2000)], false, 4000, 4000);

	assertEquals(lane.shapes.some((s) => s.style === "track" || s.style === "rendered"), false);
});

Deno.test("trackLane leaves out groups that ended before the span", () => {
	const lane = trackLane([group(0, 0, 500), group(1, 3000, 3500)], false, 4000, 2000);

	assertEquals(lane.shapes.map((s) => s.group), [1]);
});

Deno.test("trackLane stacks a busy track into columns", () => {
	// 200 one-frame groups over a second, one in fifty skipped.
	const groups = Array.from({ length: 200 }, (_, i) =>
		group(i, i * 5, i * 5 + 1, {
			frames: 1,
			rendered: i % 50 === 0 ? 0 : 1,
			state: i % 50 === 0 ? "skipped" : "complete",
		}));

	const lane = trackLane(groups, true, 60_000, 60_000);

	// Everything lands in the first of sixty columns.
	assertEquals(lane.shapes.map((s) => s.style), ["track", "complete", "skipped", "rendered"]);
	assertEquals(
		lane.shapes[1]?.title,
		[
			"200 groups arrived in this second",
			"196 received in full, 4 skipped",
			"196 of 200 frames played",
		].join("\n"),
	);
});

Deno.test("audioLane graphs how low the buffer got and marks each glitch", () => {
	const history = [
		sample(0, { buffered: 150, low: 100 }),
		sample(1000, { buffered: 200, low: 200 }),
		sample(2000, { buffered: 90, low: 50, starved: 30 }),
	];

	const lane = audioLane(history, [], 2000, 2000);

	assertEquals(lane.level, [[0, 12], [0.5, 0], [1, 18]]);
	assertEquals(lane.shapes.map((s) => [s.style, s.x0, s.title]), [
		["glitch", 1, "Sound was lost here\n30 ms of silence while the audio buffer refilled"],
	]);
});

Deno.test("audioLane shows where audio was not arriving and where it was held back", () => {
	const delays = [
		{ track: "audio", kind: "arrival", at: 1000, duration: 200 },
		{ track: "audio", kind: "held", at: 1500, duration: 100 },
		{ track: "video", kind: "arrival", at: 1800, duration: 300 },
	] as const;

	const lane = audioLane([], delays, 2000, 2000);

	assertEquals(drawn(lane), ["arrival@40-50/0", "held@70-75/4"]);
	assertEquals(lane.shapes[0]?.title, "No audio arrived for 200 ms");
});

Deno.test("shapeAt finds the topmost titled shape under a point", () => {
	const lane = trackLane(
		[group(0, 0, 2000, { rendered: 1, frames: 1, renderStart: 0, renderEnd: 2000 })],
		true,
		4000,
		4000,
	);

	const onBar = shapeAt(lane, 100, 5, 400, 2);
	const onStrip = shapeAt(lane, 100, 17, 400, 2);
	const onNothing = shapeAt(lane, 300, 5, 400, 2);

	assertEquals([onBar?.style, onStrip?.style, onNothing], ["complete", "rendered", undefined]);
});

Deno.test("shapeAt gives a mark with no width something to point at", () => {
	const lane = trackLane([group(0, 1000, 1500, { state: "skipped" })], true, 4000, 4000);

	const hit = shapeAt(lane, 101, 17, 400, 3);

	assertEquals(hit?.style, "unrendered");
});

Deno.test("withBands draws a stop behind the lane's marks and the marker over them", () => {
	const lane = {
		height: 20,
		shapes: [{ x0: 0.5, x1: 0.6, y: 0, height: 10, style: "complete" }],
	} as const;

	const banded = withBands(
		lane,
		[{ from: 1000, to: 2000, style: "marker" }, { from: 0, to: 1000, style: "stall" }],
		4000,
		4000,
	);

	assertEquals(banded.shapes, [
		{ x0: 0, x1: 0.25, y: 0, height: 20, style: "stall", title: undefined, at: [0, 1000] },
		{ x0: 0.5, x1: 0.6, y: 0, height: 10, style: "complete" },
		{
			x0: 0.25,
			x1: 0.5,
			y: 0,
			height: 20,
			style: "marker",
			title: undefined,
			at: [1000, 2000],
		},
	]);
});

Deno.test("withBands draws a stop over the lane's own background", () => {
	const lane = {
		height: 20,
		shapes: [
			{ x0: 0, x1: 1, y: 14, height: 6, style: "track" },
			{ x0: 0.5, x1: 0.6, y: 0, height: 10, style: "complete" },
		],
	} as const;

	const banded = withBands(lane, [{ from: 0, to: 1000, style: "stall" }], 4000, 4000);

	assertEquals(banded.shapes.map((s) => s.style), ["track", "stall", "complete"]);
});

Deno.test("withBands leaves out a band that ended before the span and cuts one that began before it", () => {
	const lane = { height: 10, shapes: [] };

	const banded = withBands(
		lane,
		[{ from: 0, to: 1000, style: "stall" }, { from: 1500, to: 3000, style: "stall" }],
		4000,
		2000,
	);

	assertEquals(banded.shapes.map((s) => [s.x0, s.x1]), [[0, 0.5]]);
});

Deno.test("stallBands turns each stop of the main thread into the stretch it lasted", () => {
	assertEquals(stallBands([{ at: 1000, duration: 200 }]), [
		{ from: 800, to: 1000, style: "stall", title: "The page stopped for 200 ms" },
	]);
});
