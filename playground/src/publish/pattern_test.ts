// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { clockText, momentAt } from "./pattern.ts";

Deno.test("clockText is the local time of day to the millisecond, zero-padded", () => {
	const cases = [
		{ name: "afternoon", date: new Date(2026, 9, 6, 14, 25, 23, 760), want: "14:25:23.760" },
		{ name: "single digits", date: new Date(2026, 0, 2, 3, 4, 5, 6), want: "03:04:05.006" },
		{ name: "midnight", date: new Date(2026, 0, 1, 0, 0, 0, 0), want: "00:00:00.000" },
	] as const;

	for (const c of cases) {
		assertEquals(clockText(c.date), c.want, c.name);
	}
});

Deno.test("momentAt counts frames from the time elapsed, at the frame rate", () => {
	const cases = [
		{ name: "the start", elapsed: 0, frameRate: 30, want: 0 },
		{ name: "just short of the second frame", elapsed: 33.3, frameRate: 30, want: 0 },
		{ name: "the second frame", elapsed: 33.4, frameRate: 30, want: 1 },
		{ name: "one second at 30", elapsed: 1000, frameRate: 30, want: 30 },
		{ name: "one second at 60", elapsed: 1000, frameRate: 60, want: 60 },
		{ name: "a minute at 24", elapsed: 60_000, frameRate: 24, want: 1440 },
	] as const;

	for (const c of cases) {
		assertEquals(momentAt(c.elapsed, 0, c.frameRate).frame, c.want, c.name);
	}
});

Deno.test("momentAt takes the second and the way through it from the time of day", () => {
	const moment = momentAt(0, 1_791_000_123_250, 30);

	assertEquals(moment.second, 1_791_000_123);
	assertEquals(moment.sinceSecond, 0.25);
});

Deno.test("momentAt stays on the clock whatever the frame count", () => {
	// Ten minutes in: the second still changes exactly where the clock does.
	const before = momentAt(600_000, 5_000_999, 60);
	const after = momentAt(600_001, 5_001_000, 60);

	assertEquals([before.second, after.second], [5000, 5001]);
});
