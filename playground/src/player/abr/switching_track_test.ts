// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals, assertStrictEquals } from "@std/assert";
import type { Frame, Group, Result, Track, TrackSource } from "../source.ts";
import { SwitchingTrack } from "./switching_track.ts";

class FakeGroup implements Group {
	readonly sequence: number;
	cancelled = false;
	constructor(sequence: number) {
		this.sequence = sequence;
	}
	async *frames(): AsyncIterable<Frame> {}
	cancel(): void {
		this.cancelled = true;
	}
}

// A subscription whose groups the test pushes, one at a time.
class FakeTrack implements Track {
	closed = false;
	readonly #queue: Result<Group>[] = [];
	#wake: (() => void) | undefined;

	push(seq: number): FakeGroup {
		const g = new FakeGroup(seq);
		this.#queue.push([g, undefined]);
		this.#wake?.();
		return g;
	}
	end(): void {
		this.#queue.push([undefined, new Error("ended")]);
		this.#wake?.();
	}
	async acceptGroup(_signal: Promise<void>): Promise<Result<Group>> {
		while (this.#queue.length === 0) {
			await new Promise<void>((resolve) => this.#wake = resolve);
		}
		return this.#queue.shift()!;
	}
	close(): void {
		this.closed = true;
		this.end();
	}
}

class FakeSource implements TrackSource {
	readonly tracks: Record<string, FakeTrack[]> = {};
	refuse = new Set<string>();
	subscribe(_broadcast: string, track: string): Promise<Result<Track>> {
		if (this.refuse.has(track)) return Promise.resolve([undefined, new Error(`no ${track}`)]);
		const t = new FakeTrack();
		(this.tracks[track] ??= []).push(t);
		return Promise.resolve([t, undefined]);
	}
	rtt(): Promise<number | undefined> {
		return Promise.resolve(undefined);
	}
	latest(track: string): FakeTrack {
		const list = this.tracks[track];
		return list[list.length - 1];
	}
}

// Lets the switching track's pumps run.
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
const never = new Promise<void>(() => {});

async function accepted(st: SwitchingTrack): Promise<number> {
	const [g, err] = await st.acceptGroup(never);
	if (g === undefined) throw err;
	return g.sequence;
}

Deno.test("SwitchingTrack hands out the first rendition's groups", async () => {
	const src = new FakeSource();
	const [st] = await SwitchingTrack.open(src, "/b", "video-360p", never);
	src.latest("video-360p").push(1);
	src.latest("video-360p").push(2);

	assertEquals([await accepted(st!), await accepted(st!)], [1, 2]);
	assertStrictEquals(st!.renditionOf(2), "video-360p");
	st!.close();
});

Deno.test("SwitchingTrack switches seamlessly at the new rendition's first group", async () => {
	const src = new FakeSource();
	const [st] = await SwitchingTrack.open(src, "/b", "video-360p", never);
	const switched: string[] = [];
	st!.onswitch = (r) => switched.push(r);
	const low = src.latest("video-360p");
	low.push(1);
	low.push(2);
	await accepted(st!);
	await accepted(st!);

	await st!.switchTo("video-720p", "seamless");
	const high = src.latest("video-720p");
	assertStrictEquals(st!.rendition, "video-720p");

	// The new rendition starts at the group the old one is still sending
	// the one before of.
	high.push(3);
	await tick();
	assertEquals(await accepted(st!), 3);
	assertEquals(switched, ["video-360p", "video-720p"]);

	// The old rendition's later groups are not wanted; one at the cutoff
	// shows everything before it was sent, and ends the old subscription.
	const late = low.push(3);
	await tick();
	assertStrictEquals(late.cancelled, true);
	assertStrictEquals(low.closed, true);

	high.push(4);
	assertEquals(await accepted(st!), 4);
	assertStrictEquals(st!.renditionOf(2), "video-360p");
	assertStrictEquals(st!.renditionOf(3), "video-720p");
	st!.close();
});

Deno.test("SwitchingTrack keeps the old rendition's groups before the cutoff", async () => {
	const src = new FakeSource();
	const [st] = await SwitchingTrack.open(src, "/b", "video-360p", never);
	const low = src.latest("video-360p");
	low.push(1);
	await accepted(st!);

	await st!.switchTo("video-720p", "seamless");
	const high = src.latest("video-720p");
	high.push(3); // group 2 is still on its way from the old rendition
	await tick();
	low.push(2);
	await tick();

	assertEquals([await accepted(st!), await accepted(st!)], [3, 2]);
	assertStrictEquals(st!.renditionOf(2), "video-360p");
	assertStrictEquals(low.closed, false, "kept until it sends a group at the cutoff");
	st!.close();
});

Deno.test("SwitchingTrack does not start the new rendition at a group already handed out", async () => {
	const src = new FakeSource();
	const [st] = await SwitchingTrack.open(src, "/b", "video-720p", never);
	const high = src.latest("video-720p");
	high.push(5);
	await accepted(st!);

	await st!.switchTo("video-360p", "immediate");
	const low = src.latest("video-360p");
	const repeat = low.push(5);
	await tick();
	assertStrictEquals(repeat.cancelled, true);
	assertStrictEquals(high.closed, false, "nothing switched yet");

	low.push(6);
	assertEquals(await accepted(st!), 6);
	assertStrictEquals(high.closed, true, "an immediate handover ends the old rendition at once");
	st!.close();
});

Deno.test("SwitchingTrack reserves the next group for the new rendition when the old one would take it", async () => {
	const src = new FakeSource();
	const [st] = await SwitchingTrack.open(src, "/b", "video-360p", never);
	const low = src.latest("video-360p");
	low.push(7);
	await accepted(st!);

	// The new rendition joins at the group the old one is playing...
	await st!.switchTo("video-720p", "seamless");
	const high = src.latest("video-720p");
	high.push(7);
	await tick();
	// ...and the next group reaches both at once, the old one first.
	const raced = low.push(8);
	await tick();
	assertStrictEquals(raced.cancelled, true, "group 8 is the new rendition's");
	assertStrictEquals(low.closed, true, "the old one has sent all before it");

	high.push(8);
	assertEquals(await accepted(st!), 8);
	assertStrictEquals(st!.renditionOf(8), "video-720p");
	assertStrictEquals(st!.playing, "video-720p");
	st!.close();
});

Deno.test("SwitchingTrack ends with the rendition being played, not a retired one", async () => {
	const src = new FakeSource();
	const [st] = await SwitchingTrack.open(src, "/b", "video-360p", never);
	const low = src.latest("video-360p");
	low.push(1);
	await accepted(st!);
	await st!.switchTo("video-720p", "seamless");
	const high = src.latest("video-720p");
	high.push(2);
	await accepted(st!);

	low.end();
	high.push(3);
	assertEquals(await accepted(st!), 3, "the retired rendition ending changes nothing");

	high.end();
	const [g, err] = await st!.acceptGroup(never);
	assertStrictEquals(g, undefined);
	assertEquals(err?.message, "ended");
});

Deno.test("SwitchingTrack stays with the rendition playing when a switch is refused", async () => {
	const src = new FakeSource();
	src.refuse.add("video-1080p");
	const [st] = await SwitchingTrack.open(src, "/b", "video-360p", never);
	src.latest("video-360p").push(1);
	await accepted(st!);

	const err = await st!.switchTo("video-1080p", "seamless");
	assertEquals(err?.message, "no video-1080p");
	assertStrictEquals(st!.rendition, "video-360p");
	src.latest("video-360p").push(2);
	assertEquals(await accepted(st!), 2);
	st!.close();
});

Deno.test("SwitchingTrack gives up a switch not yet delivering for a newer one", async () => {
	const src = new FakeSource();
	const [st] = await SwitchingTrack.open(src, "/b", "video-360p", never);
	src.latest("video-360p").push(1);
	await accepted(st!);

	await st!.switchTo("video-720p", "seamless");
	const abandoned = src.latest("video-720p");
	await st!.switchTo("video-1080p", "seamless");
	assertStrictEquals(abandoned.closed, true);

	src.latest("video-1080p").push(2);
	assertEquals(await accepted(st!), 2);
	assertStrictEquals(st!.renditionOf(2), "video-1080p");
	st!.close();
});

Deno.test("SwitchingTrack stops waiting when the signal resolves", async () => {
	const src = new FakeSource();
	let stop = () => {};
	const signal = new Promise<void>((resolve) => stop = resolve);
	const [st] = await SwitchingTrack.open(src, "/b", "video-360p", signal);
	const waiting = st!.acceptGroup(signal);
	stop();
	const [g, err] = await waiting;
	assertStrictEquals(g, undefined);
	assertEquals(err?.message, "stopped");
	// Asked again with the same signal, it answers at once.
	assertEquals((await st!.acceptGroup(signal))[1]?.message, "stopped");
	st!.close();
});
