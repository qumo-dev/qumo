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
	fails: Error | undefined;
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

	/** Makes writes succeed from now on. */
	heal(): void {
		this.fails = undefined;
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

// One frame to send: what it is called in the assertions, and what kind it is.
type Sent = readonly [frame: string, info: FrameInfo];

const k = (frame: string, timestamp = 0): Sent => [frame, {
	key: true,
	timestamp,
	bytes: 10,
}];
const d = (frame: string, timestamp = 0): Sent => [frame, {
	key: false,
	timestamp,
	bytes: 5,
}];

// Sends the frames one after another, each once the one before has been written.
async function sendInTurn(fanout: Fanout<string>, frames: readonly Sent[]): Promise<void> {
	for (const [frame, info] of frames) await fanout.send(frame, info);
}

// Sends the frames in one go, as an encoder does when it emits them together.
function sendTogether(fanout: Fanout<string>, frames: readonly Sent[]): Promise<void> {
	return Promise.all(frames.map(([frame, info]) => fanout.send(frame, info))).then(() => {});
}

// A promise and the function that resolves it.
function gate(): { held: Promise<void>; open: () => void } {
	let open = () => {};
	const held = new Promise<void>((resolve) => {
		open = resolve;
	});
	return { held, open };
}

// How the observer was told each group ended, in order.
function endings(observer: FakeObserver): string[] {
	return observer.calls.filter((call) => /^(complete|aborted|stopped) /.test(call));
}

Deno.test("a keyframe opens a group that the frames up to the next keyframe follow in", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	fanout.add("video", track);

	await sendInTurn(fanout, [k("k1"), d("d1"), d("d2"), k("k2"), d("d3")]);

	assertEquals(track.written, [["k1", "d1", "d2"], ["k2", "d3"]]);
});

Deno.test("a group is closed when the next keyframe opens another", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	fanout.add("video", track);

	await sendInTurn(fanout, [k("k1"), d("d1"), k("k2")]);

	assertEquals(track.groups.map((g) => g.closed), [true, false]);
});

Deno.test("a subscriber is sent nothing until the first keyframe after it arrived", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	fanout.add("video", track);

	await sendInTurn(fanout, [d("d1"), d("d2"), k("k1")]);

	assertEquals(track.written, [["k1"]]);
});

Deno.test("with frame grouping every frame is a group by itself", async () => {
	const fanout = new Fanout<string>({ grouping: "frame" });
	const track = new FakeTrack();
	fanout.add("audio", track);

	await sendInTurn(fanout, [k("a1"), k("a2")]);

	assertEquals(track.written, [["a1"], ["a2"]]);
});

Deno.test("with frame grouping a group is closed once its frame is written", async () => {
	const fanout = new Fanout<string>({ grouping: "frame" });
	const track = new FakeTrack();
	fanout.add("audio", track);

	await sendInTurn(fanout, [k("a1"), k("a2")]);

	assertEquals(track.groups.map((g) => g.closed), [true, true]);
});

Deno.test("a frame sent with no subscriber is dropped", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	await sendInTurn(fanout, [k("k1")]);
	fanout.add("video", track);

	await sendInTurn(fanout, [d("d1")]);

	assertEquals(track.written, []);
});

Deno.test("frames keep their order when a group is slow to open", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	const { held, open } = gate();
	track.hold = held;
	fanout.add("video", track);

	const sent = sendTogether(fanout, [k("k1"), d("d1"), d("d2")]);
	open();
	await sent;

	assertEquals(track.written, [["k1", "d1", "d2"]]);
});

Deno.test("a subscriber whose group cannot be opened does not stop the others", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const broken = new FakeTrack();
	broken.openError = new Error("gone");
	const working = new FakeTrack();
	fanout.add("video #1", broken);
	fanout.add("video #2", working);

	await sendInTurn(fanout, [k("k1"), d("d1")]);

	assertEquals(working.written, [["k1", "d1"]]);
});

Deno.test("a frame that could not be sent is reported with the subscriber's name", async () => {
	const errors: string[] = [];
	const fanout = new Fanout<string>({
		grouping: "keyframe",
		onerror: (track, err) => errors.push(`${track}: ${err.message}`),
	});
	const broken = new FakeTrack();
	broken.openError = new Error("gone");
	fanout.add("video #1", broken);

	await sendInTurn(fanout, [k("k1")]);

	assertEquals(errors, ["video #1: gone"]);
});

Deno.test("after a write fails the rest of the group is dropped", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	track.writeError = new Error("reset");
	fanout.add("video", track);
	await sendInTurn(fanout, [k("k1")]);
	// Even a group that would now take the frames is not written to again.
	track.groups[0]?.heal();

	await sendInTurn(fanout, [d("d1"), d("d2")]);

	assertEquals(track.written, [[]]);
});

Deno.test("the keyframe after a failed write starts a new group", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	track.writeError = new Error("reset");
	fanout.add("video", track);
	await sendInTurn(fanout, [k("k1")]);
	track.writeError = undefined;

	await sendInTurn(fanout, [d("d1"), k("k2"), d("d2")]);

	assertEquals(track.written, [[], ["k2", "d2"]]);
});

Deno.test("a removed subscriber is sent nothing more", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const track = new FakeTrack();
	fanout.add("video", track);
	await sendInTurn(fanout, [k("k1")]);
	fanout.remove(track);

	await sendInTurn(fanout, [d("d1"), k("k2")]);

	assertEquals(track.written, [["k1"]]);
});

Deno.test("size counts the subscribers there are", () => {
	const fanout = new Fanout<string>({ grouping: "keyframe" });
	const first = new FakeTrack();
	fanout.add("video #1", first);
	fanout.add("video #2", new FakeTrack());

	fanout.remove(first);

	assertEquals(fanout.size, 1);
});

Deno.test("a subscriber too far behind has the frames that do not fit dropped", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe", maxPending: 2 });
	const track = new FakeTrack();
	const { held, open } = gate();
	track.hold = held;
	fanout.add("video", track);

	// Two wait behind the group being opened; the third and fourth do not fit.
	const sent = sendTogether(fanout, [k("k1"), d("d1"), d("d2"), d("d3")]);
	open();
	await sent;

	assertEquals(track.written, [["k1", "d1"]]);
});

Deno.test("after frames were dropped, the ones that follow wait for a keyframe", async () => {
	const fanout = new Fanout<string>({ grouping: "keyframe", maxPending: 1 });
	const track = new FakeTrack();
	fanout.add("video", track);
	// The second is dropped: only one may wait.
	await sendTogether(fanout, [k("k1"), d("d1")]);

	await sendInTurn(fanout, [d("d2"), k("k2"), d("d3")]);

	assertEquals(track.written, [["k1"], ["k2", "d3"]]);
});

Deno.test("the observer is told of each group and frame, under the subscriber's name", async () => {
	const observer = new FakeObserver();
	const fanout = new Fanout<string>({ grouping: "keyframe", observer });
	const track = new FakeTrack();
	fanout.add("video sent #1", track);

	await sendInTurn(fanout, [k("k1", 1000), d("d1", 2000), k("k2", 3000)]);

	assertEquals(observer.calls, [
		"sent video sent #1",
		"open video sent #1 1",
		"frame video sent #1 1 1000 10",
		"frame video sent #1 1 2000 5",
		"complete video sent #1 1",
		"open video sent #1 2",
		"frame video sent #1 2 3000 10",
	]);
});

Deno.test("the observer is told a group was stopped when its subscriber is removed", async () => {
	const observer = new FakeObserver();
	const fanout = new Fanout<string>({ grouping: "keyframe", observer });
	const track = new FakeTrack();
	fanout.add("video", track);
	await sendInTurn(fanout, [k("k1")]);

	fanout.remove(track);

	assertEquals(endings(observer), ["stopped video 1"]);
});

Deno.test("the observer is told a group was aborted when a write to it fails", async () => {
	const observer = new FakeObserver();
	const fanout = new Fanout<string>({ grouping: "keyframe", observer });
	const track = new FakeTrack();
	track.writeError = new Error("reset");
	fanout.add("video", track);

	await sendInTurn(fanout, [k("k1")]);

	assertEquals(endings(observer), ["aborted video 1"]);
});

Deno.test("the observer is told a group that lost frames was aborted", async () => {
	const observer = new FakeObserver();
	const fanout = new Fanout<string>({ grouping: "keyframe", observer, maxPending: 1 });
	const track = new FakeTrack();
	fanout.add("video", track);
	// The second is dropped: only one may wait.
	await sendTogether(fanout, [k("k1"), d("d1")]);

	await sendInTurn(fanout, [k("k2")]);

	assertEquals(endings(observer), ["aborted video 1"]);
});

Deno.test("a dropped keyframe leaves the group before it whole", async () => {
	const observer = new FakeObserver();
	const fanout = new Fanout<string>({ grouping: "keyframe", observer, maxPending: 1 });
	const track = new FakeTrack();
	fanout.add("video", track);
	await sendInTurn(fanout, [k("k1")]);
	// The keyframe is dropped while the frame before it is still being written.
	await sendTogether(fanout, [d("d1"), k("k2")]);

	await sendInTurn(fanout, [d("d2"), k("k3")]);

	assertEquals(endings(observer), ["complete video 1"]);
});
