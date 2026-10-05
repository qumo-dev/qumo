// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { FrameLimiter, KeyframeCadence, Rebase } from "./timing.ts";

Deno.test("Rebase puts the first timestamp where the run's clock stands", () => {
	const rebase = new Rebase();

	assertEquals(rebase.at(9_000_000, 250_000), 250_000);
});

Deno.test("Rebase keeps the source's spacing, not the arrival times", () => {
	const rebase = new Rebase();
	rebase.at(9_000_000, 250_000);

	const second = rebase.at(9_033_333, 301_000);

	assertEquals(second, 283_333);
});

Deno.test("Rebase never goes back when the source's clock does", () => {
	const rebase = new Rebase();
	rebase.at(1_000_000, 0);
	rebase.at(1_033_000, 33_000);

	const steppedBack = rebase.at(1_020_000, 66_000);

	assertEquals(steppedBack, 33_001);
});

Deno.test("FrameLimiter lets a source at the wanted rate through, jitter and all", () => {
	const limiter = new FrameLimiter(30);
	const timestamps = [0, 31_000, 66_000, 99_500, 133_333];

	const taken = timestamps.map((t) => limiter.takes(t));

	assertEquals(taken, [true, true, true, true, true]);
});

Deno.test("FrameLimiter drops every other frame of a source at twice the rate", () => {
	const limiter = new FrameLimiter(30);
	const timestamps = [0, 16_667, 33_333, 50_000, 66_667];

	const taken = timestamps.map((t) => limiter.takes(t));

	assertEquals(taken, [true, false, true, false, true]);
});

Deno.test("KeyframeCadence makes the first frame a keyframe, then one each interval", () => {
	const cadence = new KeyframeCadence(1_000_000);
	const timestamps = [500_000, 533_333, 1_499_999, 1_500_000, 1_533_333, 2_600_000];

	const keys = timestamps.map((t) => cadence.isKey(t));

	assertEquals(keys, [true, false, false, true, false, true]);
});
