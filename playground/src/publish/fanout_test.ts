// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import {
	Fanout,
	type FrameInfo,
	type GroupSink,
	type SendObserver,
	type TrackSink,
} from "./fanout.ts";

// A group that keeps what is written to it. `fails` makes every write fail.
class FakeGroup implements GroupSink<string> {
	readonly frames: string[] = [];
	readonly sequence: number;
	readonly fails: Error | undefined;
	closed = false;

	constructor(sequence: number, fails: Error | undefined) {
		this.sequence = sequence;
		this.fails = fails;
	}

	writeFrame(frame: string): Promise<Error | undefined> {
		if (this.fails !== undefined) return Promise.resolve(this.fails);
		this.frames.push(frame);
		return Promise.resolve(undefined);
	}

	close(): Promise<void> {
		this.closed = true;
		return Promise.resolve();
	}
}

// A subscriber that keeps the groups opened on it.
class FakeTrack implements TrackSink<string> {
	readonly groups: FakeGroup[] = [];
	/** Set to make openGroup fail. */
	openError: Error | undefined;
	/** Set to make every write on groups opened from now on fail. */
	writeError: Error | undefined;
	/** Set to make openGroup wait for it. */
	hold: Promise<void> | undefined;

	async openGroup(): Promise<readonly [FakeGroup, undefined] | readonly [undefined, Error]> {
		await this.hold;
		if (this.openError !== undefined) return [undefined, this.openError];
		const group = new FakeGroup(this.groups.length + 1, this.writeError);
		this.groups.push(group);
		return [group, undefined];
	}

	/** The frames of each group, in the order the groups were opened. */
	get written(): string[][] {
		return this.groups.map((group) => group.frames);
	}
}

// An observer that keeps what it is told, one line per call.
class FakeObserver implements SendObserver {
	readonly calls: string[] = [];

	markSent(track: string): void {
		this.calls.push(`sent ${track}`);
	}

	groupArrived(track: string, group: number): void {
		this.calls.push(`open ${track} ${group}`);
	}

	frameArrived(track: string, group: number, timestamp: number, bytes: number): void {
		this.calls.push(`frame ${track} ${group} ${timestamp} ${bytes}`);
	}

	groupEnded(track: string, group: number, outcome: string): void {
		this.calls.push(`${outcome} ${track} ${group}`);
	}
}

const key = (timestamp = 0): FrameInfo => ({ key: true, timestamp, bytes: 10 });
const delta = (timestamp = 0): FrameInfo => ({ key: false, timestamp, bytes: 5 });

// A promise and the function that resolves it.
function gate(): { held: Promise<void>; open: () => void } {
	let open = () => {};
	const held = new Promise<void>((resolve) => {
		open = resolve;
	});
	return { held, open };
}

Deno.test("a keyframe opens a group and the frames up to the next one follow in it", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	fanout.add("video", track);

	await fanout.send("k1", key());
	await fanout.send("d1", delta());
	await fanout.send("d2", delta());
	await fanout.send("k2", key());
	await fanout.send("d3", delta());

	assertEquals(track.written, [["k1", "d1", "d2"], ["k2", "d3"]]);
	assertEquals(track.groups.map((g) => g.closed), [true, false]);
});

Deno.test("a subscriber is sent nothing until the first keyframe after it arrived", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	fanout.add("video", track);

	await fanout.send("d1", delta());
	await fanout.send("d2", delta());
	await fanout.send("k1", key());

	assertEquals(track.written, [["k1"]]);
});

Deno.test("with frame grouping every frame is a group by itself, closed once written", async () => {
	const fanout = new Fanout<string>({ grouping: "frame" });
	const track = new FakeTrack();
	fanout.add("audio", track);

	await fanout.send("a1", key());
	await fanout.send("a2", key());

	assertEquals(track.written, [["a1"], ["a2"]]);
	assertEquals(track.groups.map((g) => g.closed), [true, true]);
});

Deno.test("a frame sent with no subscriber is dropped", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();

	await fanout.send("k1", key());
	fanout.add("video", track);
	await fanout.send("d1", delta());

	assertEquals(track.written, []);
});

Deno.test("frames keep their order when a group is slow to open", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	const { held, open } = gate();
	track.hold = held;
	fanout.add("video", track);

	const sent = [fanout.send("k1", key()), fanout.send("d1", delta()), fanout.send("d2", delta())];
	open();
	await Promise.all(sent);

	assertEquals(track.written, [["k1", "d1", "d2"]]);
});

Deno.test("one subscriber failing does not stop the others", async () => {
	const errors: string[] = [];
	const fanout = new Fanout<string>({
		grouping: "keyframe",
		onerror: (track, err) => errors.push(`${track}: ${err.message}`),
	});
	const broken = new FakeTrack();
	broken.openError = new Error("gone");
	const working = new FakeTrack();
	fanout.add("video #1", broken);
	fanout.add("video #2", working);

	await fanout.send("k1", key());
	await fanout.send("d1", delta());

	assertEquals(working.written, [["k1", "d1"]]);
	assertEquals(errors, ["video #1: gone"]);
});

Deno.test("after a write fails the rest of the group is dropped, and the next keyframe starts again", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	track.writeError = new Error("reset");
	fanout.add("video", track);

	await fanout.send("k1", key());
	track.writeError = undefined;
	await fanout.send("d1", delta());
	await fanout.send("k2", key());
	await fanout.send("d2", delta());

	assertEquals(track.written, [[], ["k2", "d2"]]);
});

Deno.test("a removed subscriber is sent nothing more", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	fanout.add("video", track);
	await fanout.send("k1", key());

	fanout.remove(track);
	await fanout.send("d1", delta());
	await fanout.send("k2", key());

	assertEquals(track.written, [["k1"]]);
	assertEquals(fanout.size, 0);
});

Deno.test("a subscriber too far behind has frames dropped up to the next keyframe", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe", maxPending: 2 });
	const track = new FakeTrack();
	const { held, open } = gate();
	track.hold = held;
	fanout.add("video", track);

	// Two wait behind the group being opened; the third and fourth do not fit.
	const sent = [
		fanout.send("k1", key()),
		fanout.send("d1", delta()),
		fanout.send("d2", delta()),
		fanout.send("d3", delta()),
	];
	open();
	await Promise.all(sent);
	// The gap is not papered over: frames after it wait for a keyframe.
	await fanout.send("d4", delta());
	await fanout.send("k2", key());
	await fanout.send("d5", delta());

	assertEquals(track.written, [["k1", "d1"], ["k2", "d5"]]);
});

Deno.test("the observer is told of each group and frame, under the subscriber's name", async () => {
	const observer = new FakeObserver();
	const fanout = new Fanout<string>({ grouping: "keyframe", observer });
	const track = new FakeTrack();
	fanout.add("video sent #1", track);

	await fanout.send("k1", key(1000));
	await fanout.send("d1", delta(2000));
	await fanout.send("k2", key(3000));
	fanout.remove(track);

	assertEquals(observer.calls, [
		"sent video sent #1",
		"open video sent #1 1",
		"frame video sent #1 1 1000 10",
		"frame video sent #1 1 2000 5",
		"complete video sent #1 1",
		"open video sent #1 2",
		"frame video sent #1 2 3000 10",
		"stopped video sent #1 2",
	]);
});

Deno.test("the observer is told how a group that was not sent whole ended", async () => {
	const cases = [
		{
			name: "a write failed",
			arrange: (track: FakeTrack) => {
				track.writeError = new Error("reset");
			},
			maxPending: 64,
			want: "aborted video 1",
		},
		{
			name: "frames were dropped from it",
			arrange: () => {},
			maxPending: 1,
			want: "aborted video 1",
		},
	] as const;

	for (const c of cases) {
		const observer = new FakeObserver();
		const fanout = new Fanout<string>({
			grouping: "keyframe",
			observer,
			maxPending: c.maxPending,
		});
		const track = new FakeTrack();
		c.arrange(track);
		fanout.add("video", track);

		// The second is dropped when only one may wait.
		await Promise.all([fanout.send("k1", key()), fanout.send("d1", delta())]);
		track.writeError = undefined;
		await fanout.send("k2", key());

		assertEquals(observer.calls.includes(c.want), true, c.name);
		assertEquals(observer.calls.includes("complete video 1"), false, c.name);
	}
});
