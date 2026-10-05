// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertStrictEquals } from "@std/assert";
import { delayFor, Sync } from "./sync.ts";

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
