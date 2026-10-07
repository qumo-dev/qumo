// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertStrictEquals } from "@std/assert";
import { delayFor, delayForJitter, raisedDelay, steppedDelay, Sync, waitBudget } from "./sync.ts";

const MS = 1000; // timestamps are microseconds

Deno.test("delayFor", async (t) => {
	const cases = [
		{
			name: "falls back to the floor when the round-trip time is unknown",
			rtt: undefined,
			want: 100,
		},
		{ name: "falls back to the floor when the round-trip time is zero", rtt: 0, want: 100 },
		{ name: "keeps the floor on a fast connection", rtt: 20, want: 100 },
		{ name: "leaves room for one retransmit on a slow connection", rtt: 200, want: 250 },
	] as const;

	for (const c of cases) {
		await t.step(c.name, () => {
			assertStrictEquals(delayFor(c.rtt), c.want);
		});
	}
});

Deno.test("raisedDelay", async (t) => {
	const cases = [
		{ name: "adds half as much again", delay: 100, want: 150 },
		{ name: "stops at the ceiling", delay: 400, want: 500 },
		{ name: "stays at the ceiling once there", delay: 500, want: 500 },
		{ name: "never lowers a delay already past the ceiling", delay: 800, want: 800 },
	] as const;

	for (const c of cases) {
		await t.step(c.name, () => {
			assertStrictEquals(raisedDelay(c.delay), c.want);
		});
	}
});

Deno.test("a raised delay moves what is due later by the difference", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);

	sync.delay = 150;

	assertStrictEquals(sync.due(40 * MS), 1190);
});

Deno.test("steppedDelay", async (t) => {
	const cases = [
		{ name: "stays put when less is wanted", current: 150, wanted: 120, want: 150 },
		{
			name: "stays put for a raise too small to be worth it",
			current: 100,
			wanted: 109,
			want: 100,
		},
		{
			name: "goes a little past a raise that is worth it",
			current: 100,
			wanted: 110,
			want: 120,
		},
		{ name: "stops at the ceiling", current: 400, wanted: 495, want: 500 },
		{ name: "does not come down from past the ceiling", current: 600, wanted: 700, want: 600 },
	] as const;

	for (const c of cases) {
		await t.step(c.name, () => {
			assertStrictEquals(steppedDelay(c.current, c.wanted), c.want);
		});
	}
});

Deno.test("delayForJitter", async (t) => {
	const cases = [
		{ name: "keeps the floor when arrivals are steady", jitter: 10, floor: 100, want: 100 },
		{
			name: "adds a fifth, a frame and the audio wait to the jitter",
			jitter: 150,
			floor: 100,
			want: 220,
		},
		{ name: "stops at the ceiling", jitter: 900, floor: 100, want: 500 },
		{
			name: "never goes under a floor that is past the ceiling",
			jitter: 0,
			floor: 600,
			want: 600,
		},
	] as const;

	for (const c of cases) {
		await t.step(c.name, () => {
			assertStrictEquals(delayForJitter(c.jitter, c.floor), c.want);
		});
	}
});

Deno.test("video may wait the whole delay for a group", () => {
	assertStrictEquals(waitBudget("video", 100), 100);
});

Deno.test("audio waits about a frame, leaving the rest of the delay buffered", () => {
	assertStrictEquals(waitBudget("audio", 100), 20);
});

Deno.test("audio never waits longer than the delay itself", () => {
	assertStrictEquals(waitBudget("audio", 10), 10);
});

Deno.test("jitter is zero while media arrives at an even pace", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);
	sync.observe(20 * MS, 1020);
	sync.observe(40 * MS, 1040);

	assertStrictEquals(sync.jitter, 0);
});

Deno.test("jitter is how late the latest arrival was against the fastest before it", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);
	sync.observe(20 * MS, 1090); // 70 ms late
	sync.observe(40 * MS, 1070); // 30 ms late

	assertStrictEquals(sync.jitter, 70);
});

Deno.test("a backlog delivered in one burst is not jitter", () => {
	const sync = new Sync(100);
	// Half a second of media, oldest first, all arriving at once.
	for (let media = 0; media <= 500; media += 20) sync.observe(media * MS, 1000);

	assertStrictEquals(sync.jitter, 0);
});

Deno.test("frames sent together show the span they were held for as jitter", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);
	// The source holds four frames and sends them at once, 80 ms after the first was made.
	for (const media of [20, 40, 60, 80]) sync.observe(media * MS, 1080);

	assertStrictEquals(sync.jitter, 60);
});

Deno.test("jitter forgets a late arrival after two windows", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);
	sync.observe(20 * MS, 1220); // 200 ms late
	sync.observe(10_000 * MS, 11_000);
	const remembered = sync.jitter;
	sync.observe(20_000 * MS, 21_000);

	assertStrictEquals(remembered, 200);
	assertStrictEquals(sync.jitter, 0);
});

Deno.test("due is undefined before anything was observed", () => {
	const sync = new Sync(100);

	assertStrictEquals(sync.due(0), undefined);
});

Deno.test("media is due its delay after it would arrive on the fastest path", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);

	const due = sync.due(40 * MS);

	assertStrictEquals(due, 1140);
});

Deno.test("a faster arrival moves the clock earlier at once", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);
	sync.observe(40 * MS, 1010); // arrived 30 ms faster than the first

	const due = sync.due(80 * MS);

	assertStrictEquals(due, 1150);
});

Deno.test("a slower arrival does not move the clock", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);
	sync.observe(40 * MS, 1090); // arrived 50 ms late

	const due = sync.due(80 * MS);

	assertStrictEquals(due, 1180);
});

Deno.test("the clock follows arrivals that stay slower for two windows", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);
	// From here on everything arrives 50 ms later than the first frame did.
	sync.observe(10_000 * MS, 11_050);
	sync.observe(20_000 * MS, 21_050);

	const due = sync.due(20_040 * MS);

	assertStrictEquals(due, 21_190);
});

Deno.test("the clock keeps the previous window's fastest arrival", () => {
	const sync = new Sync(100);
	sync.observe(0, 1000);
	sync.observe(10_000 * MS, 11_050);

	const due = sync.due(10_040 * MS);

	assertStrictEquals(due, 11_140);
});
