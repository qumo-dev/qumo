// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals, assertStrictEquals } from "@std/assert";
import { Recorder } from "./recorder.ts";

// A recorder on a clock the test moves by hand.
function recording(): { recorder: Recorder; clock: { now: number } } {
	const clock = { now: 0 };
	return { recorder: new Recorder(() => clock.now), clock };
}

Deno.test("a group that has arrived is receiving until it ends", () => {
	const { recorder, clock } = recording();
	clock.now = 10;
	recorder.groupArrived("video", 7);

	const [video] = recorder.snapshot();

	assertEquals(video?.groups, [{
		sequence: 7,
		arrived: 10,
		ended: undefined,
		state: "receiving",
		frames: 0,
		bytes: 0,
		rendered: 0,
		renderStart: undefined,
		renderEnd: undefined,
	}]);
});

Deno.test("frames add to their group and to the track", () => {
	const { recorder } = recording();
	recorder.groupArrived("video", 0);
	recorder.frameArrived("video", 0, 0, 1000);
	recorder.frameArrived("video", 0, 33_000, 200);

	const [video] = recorder.snapshot();

	assertStrictEquals(video?.groups[0]?.frames, 2);
	assertStrictEquals(video?.groups[0]?.bytes, 1200);
	assertStrictEquals(video?.frames, 2);
	assertStrictEquals(video?.bytes, 1200);
});

Deno.test("an ended group records how and when it ended", async (t) => {
	for (const outcome of ["complete", "aborted", "skipped", "late", "stopped"] as const) {
		await t.step(outcome, () => {
			const { recorder, clock } = recording();
			recorder.groupArrived("video", 0);
			clock.now = 500;

			recorder.groupEnded("video", 0, outcome);

			const [video] = recorder.snapshot();
			assertStrictEquals(video?.groups[0]?.state, outcome);
			assertStrictEquals(video?.groups[0]?.ended, 500);
			assertStrictEquals(video?.ended[outcome], 1);
		});
	}
});

Deno.test("a rendered frame is credited to the group it arrived in", () => {
	const { recorder, clock } = recording();
	recorder.groupArrived("video", 0);
	recorder.groupArrived("video", 1);
	recorder.frameArrived("video", 0, 0, 100);
	recorder.frameArrived("video", 1, 2_000_000, 100);
	clock.now = 120;

	recorder.frameRendered("video", 2_000_000);

	const [video] = recorder.snapshot();
	assertStrictEquals(video?.groups[0]?.rendered, 0);
	assertStrictEquals(video?.groups[1]?.rendered, 1);
	assertStrictEquals(video?.groups[1]?.renderStart, 120);
	assertStrictEquals(video?.rendered, 1);
});

Deno.test("the render span runs from the first rendered frame to the latest", () => {
	const { recorder, clock } = recording();
	recorder.groupArrived("video", 0);
	recorder.frameArrived("video", 0, 0, 100);
	recorder.frameArrived("video", 0, 33_000, 100);
	clock.now = 100;
	recorder.frameRendered("video", 0);
	clock.now = 133;

	recorder.frameRendered("video", 33_000);

	const [video] = recorder.snapshot();
	assertStrictEquals(video?.groups[0]?.renderStart, 100);
	assertStrictEquals(video?.groups[0]?.renderEnd, 133);
});

Deno.test("a frame rendered twice is counted once", () => {
	const { recorder } = recording();
	recorder.groupArrived("video", 0);
	recorder.frameArrived("video", 0, 0, 100);
	recorder.frameRendered("video", 0);

	recorder.frameRendered("video", 0);

	const [video] = recorder.snapshot();
	assertStrictEquals(video?.rendered, 1);
});

Deno.test("a group that ended keeps its first outcome", () => {
	const { recorder } = recording();
	recorder.groupArrived("video", 0);
	recorder.groupEnded("video", 0, "aborted");

	recorder.groupEnded("video", 0, "complete");

	const [video] = recorder.snapshot();
	assertStrictEquals(video?.groups[0]?.state, "aborted");
	assertEquals([video?.ended.aborted, video?.ended.complete], [1, 0]);
});

Deno.test("a track renders unless it is marked as sent", () => {
	const { recorder } = recording();
	recorder.groupArrived("video", 0);
	recorder.markSent("video sent");

	const tracks = recorder.snapshot();

	assertEquals(tracks.map((t) => [t.name, t.renders]), [["video", true], ["video sent", false]]);
});

const QUIET = {
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
};

Deno.test("keeps the audio buffer's reports with the time each came in", () => {
	const { recorder, clock } = recording();
	clock.now = 100;
	recorder.audioBuffer(QUIET);
	clock.now = 200;
	recorder.audioBuffer({ ...QUIET, buffered: 80 });

	const history = recorder.audio();

	assertEquals(history.map((s) => [s.at, s.buffered]), [[100, 100], [200, 80]]);
});

Deno.test("forgets audio buffer reports older than a minute", () => {
	const { recorder, clock } = recording();
	recorder.audioBuffer(QUIET);
	clock.now = 60_001;

	recorder.audioBuffer(QUIET);

	assertEquals(recorder.audio().map((s) => s.at), [60_001]);
});

Deno.test("records a stretch in which nothing of a track arrived", () => {
	const { recorder, clock } = recording();
	recorder.groupArrived("audio", 0);
	recorder.frameArrived("audio", 0, 0, 100);
	clock.now = 119;
	recorder.frameArrived("audio", 0, 21_000, 100);
	clock.now = 300;

	recorder.frameArrived("audio", 0, 42_000, 100);

	assertEquals(recorder.delays(), [{ track: "audio", kind: "arrival", at: 300, duration: 181 }]);
});

Deno.test("the time spent stopped is not media that failed to arrive", () => {
	const { recorder, clock } = recording();
	recorder.groupArrived("audio", 0);
	recorder.frameArrived("audio", 0, 0, 100);
	clock.now = 5000;

	recorder.playbackStarted();
	recorder.frameArrived("audio", 0, 21_000, 100);

	assertEquals(recorder.delays(), []);
});

Deno.test("the audio buffer's history starts again with each playback", () => {
	const { recorder } = recording();
	recorder.audioBuffer(QUIET);

	recorder.playbackStarted();

	assertEquals(recorder.audio(), []);
});

Deno.test("records a frame held back in the player", () => {
	const { recorder, clock } = recording();
	clock.now = 700;

	recorder.frameHeld("audio", 85);

	assertEquals(recorder.delays(), [{ track: "audio", kind: "held", at: 700, duration: 85 }]);
});

Deno.test("keeps the main thread's stops with the time each ended", () => {
	const { recorder, clock } = recording();
	clock.now = 500;
	recorder.mainThreadStalled(120);

	assertEquals(recorder.stalls(), [{ at: 500, duration: 120 }]);
});

Deno.test("forgets main thread stops older than a minute", () => {
	const { recorder, clock } = recording();
	recorder.mainThreadStalled(120);
	clock.now = 60_001;

	recorder.mainThreadStalled(40);

	assertEquals(recorder.stalls().map((s) => s.duration), [40]);
});

Deno.test("tracks are kept apart", () => {
	const { recorder } = recording();
	recorder.groupArrived("video", 0);
	recorder.groupArrived("audio", 0);
	recorder.frameArrived("audio", 0, 0, 50);

	const tracks = recorder.snapshot();

	assertEquals(tracks.map((t) => [t.name, t.frames]), [["video", 0], ["audio", 1]]);
});

Deno.test("a group is forgotten a minute after it ended, but stays in the totals", () => {
	const { recorder, clock } = recording();
	recorder.groupArrived("video", 0);
	recorder.frameArrived("video", 0, 0, 100);
	recorder.groupEnded("video", 0, "complete");
	clock.now = 60_001;

	const [video] = recorder.snapshot();

	assertEquals(video?.groups, []);
	assertStrictEquals(video?.ended.complete, 1);
	assertStrictEquals(video?.frames, 1);
	assertStrictEquals(video?.latest, 0);
});

Deno.test("a group still being received is kept however old it is", () => {
	const { recorder, clock } = recording();
	recorder.groupArrived("video", 0);
	clock.now = 300_000;

	const [video] = recorder.snapshot();

	assertStrictEquals(video?.groups.length, 1);
});

Deno.test("the latest group is the highest sequence seen, not the last to arrive", () => {
	const { recorder } = recording();
	recorder.groupArrived("audio", 5);
	recorder.groupArrived("audio", 4);

	const [audio] = recorder.snapshot();

	assertStrictEquals(audio?.latest, 5);
});
