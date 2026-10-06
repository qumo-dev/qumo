// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { clockText } from "./pattern.ts";

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
