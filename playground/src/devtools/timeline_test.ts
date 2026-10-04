// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import type { GroupRecord, GroupState } from "./recorder.ts";
import { columns, packLanes } from "./timeline.ts";

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
