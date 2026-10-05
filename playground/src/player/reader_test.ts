// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals, assertInstanceOf, assertRejects, assertStrictEquals } from "@std/assert";
import { FakeTime } from "@std/testing/time";
import {
	type GroupObserver,
	type GroupOutcome,
	readFrames,
	TrackEndedError,
	type TrackFrame,
} from "./reader.ts";
import type { Frame, Group, Result, Track } from "./source.ts";

const MS = 1000; // timestamps are microseconds

// A frame in these tests is its timestamp, as one float64.
function pack(timestamp: number): Uint8Array {
	const bytes = new Uint8Array(8);
	new DataView(bytes.buffer).setFloat64(0, timestamp);
	return bytes;
}

function unpack(bytes: Uint8Array): { timestamp: number; data: Uint8Array } {
	const timestamp = new DataView(bytes.buffer, bytes.byteOffset).getFloat64(0);
	return { timestamp, data: bytes.slice() };
}

// A queue whose reader waits for the writer.
class Feed<T> {
	#items: T[] = [];
	#wake: (() => void) | undefined;

	push(item: T): void {
		this.#items.push(item);
		this.wake();
	}

	wake(): void {
		const wake = this.#wake;
		this.#wake = undefined;
		wake?.();
	}

	take(): T | undefined {
		return this.#items.shift();
	}

	wait(): Promise<void> {
		return new Promise((resolve) => {
			this.#wake = resolve;
		});
	}
}

type GroupEvent = { frame: Uint8Array } | { end: true } | { abort: Error };

class FakeGroup implements Group {
	readonly sequence: number;
	cancelled = false;
	#feed = new Feed<GroupEvent>();

	constructor(sequence: number) {
		this.sequence = sequence;
	}

	/** Adds frames with the given timestamps, in milliseconds. */
	frame(...timestamps: number[]): this {
		for (const t of timestamps) this.#feed.push({ frame: pack(t * MS) });
		return this;
	}

	end(): this {
		this.#feed.push({ end: true });
		return this;
	}

	abort(err: Error): this {
		this.#feed.push({ abort: err });
		return this;
	}

	async *frames(): AsyncGenerator<Frame> {
		while (true) {
			if (this.cancelled) throw new Error("group cancelled");
			const event = this.#feed.take();
			if (event === undefined) {
				await this.#feed.wait();
			} else if ("frame" in event) {
				yield { bytes: event.frame };
			} else if ("end" in event) {
				return;
			} else {
				throw event.abort;
			}
		}
	}

	cancel(): void {
		this.cancelled = true;
		this.#feed.wake();
	}
}

class FakeTrack implements Track {
	closed = false;
	#feed = new Feed<FakeGroup>();
	#ended: Error | undefined;

	/** Makes the groups available to acceptGroup, in the order given. */
	deliver(...groups: FakeGroup[]): void {
		for (const group of groups) this.#feed.push(group);
	}

	/** Stops the track: once the delivered groups are accepted, acceptGroup fails with `err`. */
	end(err: Error): void {
		this.#ended = err;
		this.#feed.wake();
	}

	async acceptGroup(signal: Promise<void>): Promise<Result<Group>> {
		let cancelled = false;
		void signal.then(() => {
			cancelled = true;
			this.#feed.wake();
		});
		while (true) {
			if (cancelled) return [undefined, new Error("cancelled")];
			const group = this.#feed.take();
			if (group !== undefined) return [group, undefined];
			if (this.#ended) return [undefined, this.#ended];
			await this.#feed.wait();
		}
	}

	close(): void {
		this.closed = true;
	}
}

// What an observer was told, as "arrived 3", "frame 3", "ended 3 complete".
class Events implements GroupObserver {
	readonly seen: string[] = [];

	groupArrived(group: number): void {
		this.seen.push(`arrived ${group}`);
	}

	frameArrived(group: number): void {
		this.seen.push(`frame ${group}`);
	}

	groupEnded(group: number, outcome: GroupOutcome): void {
		this.seen.push(`ended ${group} ${outcome}`);
	}
}

// Drives readFrames over a FakeTrack on a clock the test moves by hand, and
// lets a test end it cleanly.
class Harness {
	readonly track = new FakeTrack();
	readonly events = new Events();
	readonly #time: FakeTime;
	readonly #frames: AsyncGenerator<TrackFrame, never>;
	#stop: () => void = () => {};
	/**
	 * The media time, in milliseconds, that is due for playback right now.
	 * While undefined the reader is given no lateness to go by.
	 */
	playhead: number | undefined;

	constructor(time: FakeTime, maxAge?: number | (() => number)) {
		this.#time = time;
		const done = new Promise<void>((resolve) => {
			this.#stop = resolve;
		});
		this.#frames = readFrames(this.track, done, {
			unpack,
			maxAge,
			observer: this.events,
			now: () => time.now,
			lateness: (timestamp) =>
				this.playhead === undefined ? -Infinity : this.playhead - timestamp / MS,
		});
	}

	/** The next `count` frames, as "group.index" labels. */
	async take(count: number): Promise<string[]> {
		const labels: string[] = [];
		for (let i = 0; i < count; i++) {
			const { value } = await this.#frames.next();
			labels.push(`${value.group}.${value.index}`);
		}
		return labels;
	}

	next(): Promise<IteratorResult<TrackFrame, never>> {
		return this.#frames.next();
	}

	/** Lets `ms` pass, then says whether `promise` is still unsettled. */
	async pendingAfter(promise: Promise<unknown>, ms: number): Promise<boolean> {
		await this.#time.tickAsync(ms);
		// A timer that fired on the last tick wakes the reader, and the frame
		// then takes a few more turns to come out of the generator.
		await this.#time.tickAsync(0);
		const pending = Symbol();
		return await Promise.race([promise, Promise.resolve(pending)]) === pending;
	}

	/** Lets everything already runnable finish, without time passing. */
	async settle(): Promise<void> {
		await this.#time.tickAsync(0);
	}

	/** Resolves `done` and waits for the reader to finish. */
	async stop(pending: Promise<unknown> = this.#frames.next()): Promise<void> {
		this.#stop();
		await assertRejects(() => pending, TrackEndedError);
	}
}

Deno.test("delivers frames in group order, then frame order", async () => {
	using time = new FakeTime();
	const h = new Harness(time);
	h.track.deliver(
		new FakeGroup(0).frame(0, 33).end(),
		new FakeGroup(1).frame(66, 100).end(),
	);

	const labels = await h.take(4);

	assertEquals(labels, ["0.0", "0.1", "1.0", "1.1"]);
	await h.stop();
});

Deno.test("delivers groups that arrive out of order in sequence order", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	h.track.deliver(
		new FakeGroup(0).frame(0).end(),
		new FakeGroup(2).frame(40).end(),
		new FakeGroup(1).frame(20).end(),
	);

	const labels = await h.take(3);

	assertEquals(labels, ["0.0", "1.0", "2.0"]);
	await h.stop();
});

Deno.test("delivers every group of a burst, however much media it spans", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	const first = new FakeGroup(0);
	h.track.deliver(
		first,
		new FakeGroup(1).frame(200).end(),
		new FakeGroup(2).frame(400).end(),
		new FakeGroup(3).frame(600).end(),
	);
	await h.settle();
	// The first group's frame is read last, though no time has passed.
	first.frame(0).end();

	const labels = await h.take(4);

	assertEquals(labels, ["0.0", "1.0", "2.0", "3.0"]);
	assertStrictEquals(first.cancelled, false);
	await h.stop();
});

Deno.test("moves on at once from a group whose frames are done when the next continues it", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	const open = new FakeGroup(0).frame(0, 33);
	h.track.deliver(open, new FakeGroup(1).frame(66));

	const labels = await h.take(3);
	await h.settle();

	assertEquals(labels, ["0.0", "0.1", "1.0"]);
	assertStrictEquals(open.cancelled, true);
	assertEquals(h.events.seen.includes("ended 0 complete"), true);
	await h.stop();
});

Deno.test("does not take a gap after the last frame for the group's end", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	const open = new FakeGroup(0).frame(0, 33);
	h.track.deliver(open, new FakeGroup(1).frame(133));
	await h.take(2);
	const next = h.next();

	const pending = await h.pendingAfter(next, 50);

	assertStrictEquals(pending, true);
	assertStrictEquals(open.cancelled, false);
	await h.stop(next);
});

Deno.test("skips whole groups that are too late when a newer one is ready", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	h.playhead = 0;
	h.track.deliver(new FakeGroup(0).frame(0).end());
	await h.take(1);
	// Playback stalls; when it resumes, two groups of backlog and a fresh one are waiting.
	h.playhead = 2000;
	const stale = [new FakeGroup(1).frame(500), new FakeGroup(2).frame(1000)];
	h.track.deliver(...stale, new FakeGroup(3).frame(1950).end());
	await h.settle();

	const labels = await h.take(1);
	await h.settle();

	assertEquals(labels, ["3.0"]);
	assertEquals(stale.map((g) => g.cancelled), [true, true]);
	assertEquals(h.events.seen.filter((e) => e.endsWith("skipped")), [
		"ended 1 skipped",
		"ended 2 skipped",
	]);
	await h.stop();
});

Deno.test("a too-late group that had already arrived in full is skipped all the same", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	h.playhead = 0;
	h.track.deliver(new FakeGroup(0).frame(0).end());
	await h.take(1);
	h.playhead = 2000;
	h.track.deliver(new FakeGroup(1).frame(500).end(), new FakeGroup(2).frame(1950).end());
	await h.settle();

	const labels = await h.take(1);

	assertEquals(labels, ["2.0"]);
	// The network did deliver it whole; only playback passed it over.
	assertEquals(h.events.seen.includes("ended 1 complete"), true);
	await h.stop();
});

Deno.test("plays a late group when nothing newer is ready", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	h.playhead = 1000;
	h.track.deliver(new FakeGroup(0).frame(0, 33));

	const labels = await h.take(2);

	assertEquals(labels, ["0.0", "0.1"]);
	await h.stop();
});

Deno.test("finishes a group it has started, however late it becomes", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	h.playhead = 0;
	h.track.deliver(new FakeGroup(0).frame(0, 33, 66).end());
	await h.take(1);
	h.playhead = 5000;
	h.track.deliver(new FakeGroup(1).frame(4990).end());
	await h.settle();

	const labels = await h.take(3);

	assertEquals(labels, ["0.1", "0.2", "1.0"]);
	await h.stop();
});

Deno.test("keeps waiting for a stalled group while newer media is within maxAge", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	const stalled = new FakeGroup(0).frame(0);
	h.track.deliver(stalled, new FakeGroup(1).frame(100));
	await h.take(1);
	const next = h.next();

	const pending = await h.pendingAfter(next, 500);

	assertStrictEquals(pending, true);
	assertStrictEquals(stalled.cancelled, false);
	await h.stop(next);
});

Deno.test("waits maxAge for a stalled group before skipping it", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	const stalled = new FakeGroup(0).frame(0);
	h.track.deliver(stalled, new FakeGroup(1).frame(500));
	await h.take(1);
	const next = h.next();

	const pendingBefore = await h.pendingAfter(next, 99);
	const pendingAfter = await h.pendingAfter(next, 1);

	assertStrictEquals(pendingBefore, true);
	assertStrictEquals(pendingAfter, false);
	assertStrictEquals((await next).value.group, 1);
	assertStrictEquals(stalled.cancelled, true);
	await h.stop();
});

Deno.test("a stalled group that produces a frame is waited for afresh", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	const slow = new FakeGroup(0).frame(0);
	h.track.deliver(slow, new FakeGroup(1).frame(500));
	await h.take(1);
	await h.pendingAfter(h.settle(), 60);
	slow.frame(33);
	await h.take(1);
	const next = h.next();

	const pending = await h.pendingAfter(next, 60);

	assertStrictEquals(pending, true);
	assertStrictEquals(slow.cancelled, false);
	await h.stop(next);
});

Deno.test("waits maxAge for a missing group before moving past it", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	h.track.deliver(
		new FakeGroup(0).frame(0).end(),
		new FakeGroup(2).frame(500),
	);
	await h.take(1);
	const next = h.next();

	const pendingBefore = await h.pendingAfter(next, 99);
	const pendingAfter = await h.pendingAfter(next, 1);

	assertStrictEquals(pendingBefore, true);
	assertStrictEquals(pendingAfter, false);
	assertStrictEquals((await next).value.group, 2);
	await h.stop();
});

Deno.test("follows a wait that is lengthened while it is running", async () => {
	using time = new FakeTime();
	let maxAge = 100;
	const h = new Harness(time, () => maxAge);
	h.track.deliver(
		new FakeGroup(0).frame(0).end(),
		new FakeGroup(2).frame(500),
	);
	await h.take(1);
	const next = h.next();

	const pendingAtFirst = await h.pendingAfter(next, 50);
	maxAge = 200;
	const pendingPastTheOldWait = await h.pendingAfter(next, 149);
	const pendingAtTheNewWait = await h.pendingAfter(next, 1);

	assertStrictEquals(pendingAtFirst, true);
	assertStrictEquals(pendingPastTheOldWait, true);
	assertStrictEquals(pendingAtTheNewWait, false);
	await h.stop();
});

Deno.test("keeps waiting for a missing group while newer media is within maxAge", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	h.track.deliver(
		new FakeGroup(0).frame(0).end(),
		new FakeGroup(2).frame(100),
	);
	await h.take(1);
	const next = h.next();

	const pending = await h.pendingAfter(next, 500);

	assertStrictEquals(pending, true);
	await h.stop(next);
});

Deno.test("cancels a group that arrives after delivery has passed it", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 0);
	h.track.deliver(
		new FakeGroup(0).frame(0).end(),
		new FakeGroup(2).frame(500),
	);
	await h.take(2);
	const late = new FakeGroup(1).frame(50).end();

	h.track.deliver(late);
	await h.settle();

	assertStrictEquals(late.cancelled, true);
	await h.stop();
});

Deno.test("with maxAge 0, skips a stalled group as soon as a newer frame exists", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 0);
	const stalled = new FakeGroup(0).frame(0);
	h.track.deliver(stalled, new FakeGroup(1).frame(1));

	const labels = await h.take(2);

	assertEquals(labels, ["0.0", "1.0"]);
	assertStrictEquals(stalled.cancelled, true);
	await h.stop();
});

Deno.test("at the start, gives up on a first group that stays empty", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	const empty = new FakeGroup(0);
	h.track.deliver(empty, new FakeGroup(1).frame(0, 101));
	const taken = h.take(2);

	await h.pendingAfter(taken, 100);

	assertEquals(await taken, ["1.0", "1.1"]);
	assertStrictEquals(empty.cancelled, true);
	await h.stop();
});

Deno.test("delivers the frames of an aborted group, then the next group", async () => {
	using time = new FakeTime();
	const h = new Harness(time);
	h.track.deliver(
		new FakeGroup(0).frame(0, 33).abort(new Error("reset")),
		new FakeGroup(1).frame(66).end(),
	);

	const labels = await h.take(3);

	assertEquals(labels, ["0.0", "0.1", "1.0"]);
	await h.stop();
});

Deno.test("throws TrackEndedError and cancels open groups when done resolves", async () => {
	using time = new FakeTime();
	const h = new Harness(time);
	const open = new FakeGroup(0).frame(0);
	h.track.deliver(open);
	await h.take(1);

	await h.stop();

	assertStrictEquals(open.cancelled, true);
});

Deno.test("delivers buffered groups before throwing when the track ends", async () => {
	using time = new FakeTime();
	const h = new Harness(time);
	const cause = new Error("publisher gone");
	h.track.deliver(new FakeGroup(0).frame(0).end(), new FakeGroup(1).frame(33).end());
	h.track.end(cause);

	const labels = await h.take(2);
	const err = await assertRejects(() => h.next(), TrackEndedError);

	assertEquals(labels, ["0.0", "1.0"]);
	assertStrictEquals(err.cause, cause);
});

Deno.test("crosses a missing group without waiting once the track has ended", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	h.track.deliver(new FakeGroup(0).frame(0).end(), new FakeGroup(2).frame(33).end());
	h.track.end(new Error("publisher gone"));

	const labels = await h.take(2);

	assertEquals(labels, ["0.0", "2.0"]);
	await assertRejects(() => h.next(), TrackEndedError);
});

Deno.test("throws the unpack error when a frame cannot be unpacked", async () => {
	const track = new FakeTrack();
	const failure = new RangeError("bad frame");
	const frames = readFrames(track, new Promise(() => {}), {
		unpack: () => {
			throw failure;
		},
	});
	track.deliver(new FakeGroup(0).frame(0));

	const err = await assertRejects(() => frames.next());

	assertInstanceOf(err, RangeError);
	assertStrictEquals(err, failure);
});

Deno.test("a frame delivered as soon as it is read was not held", async () => {
	using time = new FakeTime();
	const h = new Harness(time);
	h.track.deliver(new FakeGroup(0).frame(0).end());

	const { value } = await h.next();

	assertStrictEquals(value.held, 0);
	await h.stop();
});

Deno.test("a frame that waited for an earlier group says for how long", async () => {
	using time = new FakeTime();
	const h = new Harness(time, 100);
	h.track.deliver(new FakeGroup(0).frame(0).end(), new FakeGroup(2).frame(500));
	await h.take(1);
	const next = h.next();

	await h.pendingAfter(next, 100);

	assertStrictEquals((await next).value.held, 100);
	await h.stop();
});

Deno.test("tells the observer of each group's arrival, frames and end", async () => {
	using time = new FakeTime();
	const h = new Harness(time);
	h.track.deliver(new FakeGroup(0).frame(0, 33).end());
	await h.take(2);

	await h.settle();

	assertEquals(h.events.seen, ["arrived 0", "frame 0", "frame 0", "ended 0 complete"]);
	await h.stop();
});

Deno.test("tells the observer how a group that was not read to its end ended", async (t) => {
	await t.step("aborted by the sender", async () => {
		using time = new FakeTime();
		const h = new Harness(time);
		h.track.deliver(new FakeGroup(0).frame(0).abort(new Error("reset")));
		await h.take(1);

		await h.settle();

		assertEquals(h.events.seen.at(-1), "ended 0 aborted");
		await h.stop();
	});

	await t.step("skipped by the reader", async () => {
		using time = new FakeTime();
		const h = new Harness(time, 0);
		h.track.deliver(new FakeGroup(0).frame(0), new FakeGroup(1).frame(1));
		await h.take(2);

		await h.settle();

		assertEquals(h.events.seen.includes("ended 0 skipped"), true);
		await h.stop();
	});

	await t.step("late", async () => {
		using time = new FakeTime();
		const h = new Harness(time, 0);
		h.track.deliver(new FakeGroup(0).frame(0).end(), new FakeGroup(2).frame(500));
		await h.take(2);

		h.track.deliver(new FakeGroup(1));
		await h.settle();

		assertEquals(h.events.seen.slice(-2), ["arrived 1", "ended 1 late"]);
		await h.stop();
	});

	await t.step("open when the reader stopped", async () => {
		using time = new FakeTime();
		const h = new Harness(time);
		h.track.deliver(new FakeGroup(0).frame(0));
		await h.take(1);

		await h.stop();
		await h.settle();

		assertEquals(h.events.seen.at(-1), "ended 0 stopped");
	});
});
