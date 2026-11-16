// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals, assertStrictEquals } from "@std/assert";
import { AbrController, type Choice } from "./controller.ts";

// 1080p (bitrate measured later), 720p at 2.5 Mbit/s, 360p at 800 kbit/s.
const LADDER: Choice[] = [{ bitrate: undefined }, { bitrate: 2_500_000 }, { bitrate: 800_000 }];
const FRAME_MS = 1000 / 30;

// A controller on a clock the test moves, and a stream of frames for it.
function setup(choices = LADDER) {
	let now = 0;
	const ctl = new AbrController(choices, { now: () => now });
	let media = 0;
	return {
		ctl,
		get now() {
			return now;
		},
		/**
		 * Plays `seconds` of 30 fps frames of `bytes` each, each arriving
		 * `delay(i)` ms after its media time.
		 */
		play(seconds: number, bytes: number, delay: (i: number) => number = () => 20) {
			const start = media;
			for (let i = 0; i < seconds * 30; i++) {
				media = start + i * FRAME_MS;
				now = Math.max(now, media + delay(i));
				ctl.frameArrived(media * 1000, bytes);
			}
			media = start + seconds * 30 * FRAME_MS;
		},
		advance(ms: number) {
			now += ms;
			media += ms;
		},
	};
}

Deno.test("AbrController starts at the cheapest rendition", () => {
	assertStrictEquals(setup().ctl.initial(), 2);
});

Deno.test("AbrController steps up one rendition after a calm stretch", () => {
	const s = setup();
	s.play(3, 3000);
	assertEquals(s.ctl.decide(2), undefined, "too soon");
	s.play(2, 3000);
	assertEquals(s.ctl.decide(2)?.to, 1);
});

Deno.test("AbrController steps down when frames fall behind, to what the link carries", () => {
	const s = setup();
	s.ctl.switched(2, 0);
	s.advance(3000);
	// 1080p at ~6 Mbit/s over a ~3 Mbit/s link: each frame lands later than the last.
	s.play(4, 25_000, (i) => 20 + i * (FRAME_MS * 1)); // arrives at half speed
	const d = s.ctl.decide(0);
	assertEquals(d?.to, 2, "3 Mbit/s with headroom carries 360p, not 720p");
	assertEquals(d?.reason.includes("late"), true);
});

Deno.test("AbrController measures the link from when frames started falling behind", () => {
	const s = setup();
	s.ctl.switched(2, 0);
	s.advance(3000);
	// 1080p at 6 Mbit/s plays fine...
	s.play(2, 25_000);
	// ...until the link drops to 3 Mbit/s: each frame lands later than the last.
	s.play(1, 25_000, (i) => 20 + i * FRAME_MS);
	// The rate over the last 2 s still includes the good second; since the
	// frames fell behind it is 3 Mbit/s, which carries 360p and not 720p.
	assertEquals(s.ctl.decide(0)?.to, 2);
});

Deno.test("AbrController steps down at least one rendition", () => {
	const s = setup();
	s.ctl.switched(2, 1);
	s.advance(3000);
	// Behind, but at a rate that would still fit 720p: step down anyway.
	s.play(2, 12_000, (i) => (i < 30 ? 20 : 20 + (i - 30) * 20));
	assertEquals(s.ctl.decide(1)?.to, 2);
});

Deno.test("AbrController steps down when a group is lost", () => {
	const s = setup();
	s.ctl.switched(2, 1);
	s.play(4, 3000);
	s.ctl.groupLost();
	assertEquals(s.ctl.decide(1)?.to, 2);
	assertEquals(s.ctl.decide(1), undefined, "a loss counts once");
});

Deno.test("AbrController makes no decision while a switch settles", () => {
	const s = setup();
	s.ctl.switched(2, 1);
	s.ctl.groupLost();
	s.advance(1000);
	assertEquals(s.ctl.decide(1), undefined);
});

Deno.test("AbrController waits twice as long after a step up does not hold", () => {
	const s = setup();
	s.play(5, 3000);
	assertEquals(s.ctl.decide(2)?.to, 1);
	s.ctl.switched(2, 1);
	s.play(3, 3000);
	s.ctl.groupLost();
	assertEquals(s.ctl.decide(1)?.to, 2);
	s.ctl.switched(1, 2);

	s.play(5, 3000);
	assertEquals(s.ctl.decide(2), undefined, "4 s is no longer enough");
	s.play(4, 3000);
	assertEquals(s.ctl.decide(2)?.to, 1);
});

Deno.test("AbrController does not count a step down by hand against a step up", () => {
	const s = setup();
	s.play(5, 3000);
	assertEquals(s.ctl.decide(2)?.to, 1);
	s.ctl.switched(2, 1);
	s.play(2, 3000);
	s.ctl.switched(1, 2, true);

	s.play(5, 3000);
	assertEquals(s.ctl.decide(2)?.to, 1, "the next step up comes after 4 s, as the first");
});

Deno.test("AbrController does not step up past what a burst measured", async (t) => {
	// A subscription's opening burst: the newest group's first 10 frames, from
	// 300 ms back, arriving back to back `gap` ms apart; then live frames.
	const opening = (gap: number) => (i: number) => i < 10 ? 320 + i * gap - i * FRAME_MS : 20;
	const cases = [
		// 9 × 2,000 bytes in 54 ms is 2.7 Mbit/s: too slow for 2.5 Mbit/s with headroom.
		{ name: "a slow burst rules the step out", gap: 6, want: undefined },
		// In 18 ms it is 8 Mbit/s.
		{ name: "a fast burst allows it", gap: 2, want: 1 },
	];
	for (const c of cases) {
		await t.step(c.name, () => {
			const s = setup();
			s.play(1, 2000, opening(c.gap));
			s.play(4, 2000);
			assertEquals(s.ctl.stats().burstRate !== undefined, true, "a burst was measured");
			assertEquals(s.ctl.decide(2)?.to, c.want);
		});
	}
});

Deno.test("AbrController measures lag against the fastest recent arrival", () => {
	const s = setup();
	s.play(1, 1000, (i) => 20 + (i === 29 ? 300 : 0));
	assertEquals(Math.round(s.ctl.lag()), 300);
});
