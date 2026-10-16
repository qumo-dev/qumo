// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { readCatalogs } from "./catalog.ts";
import type { Frame, Group, Result, Track } from "./source.ts";

class FakeGroup implements Group {
	readonly sequence: number;
	readonly payload: string;
	cancelled = false;

	constructor(sequence: number, payload: string) {
		this.sequence = sequence;
		this.payload = payload;
	}

	async *frames(): AsyncIterable<Frame> {
		yield { bytes: new TextEncoder().encode(this.payload) };
	}

	cancel(): void {
		this.cancelled = true;
	}
}

// A track that hands out its groups in the order given, then fails with
// `ended`.
class FakeTrack implements Track {
	readonly ended = new Error("track ended");
	#groups: FakeGroup[];

	constructor(groups: FakeGroup[]) {
		this.#groups = [...groups];
	}

	async acceptGroup(_signal: Promise<void>): Promise<Result<Group>> {
		const group = this.#groups.shift();
		return group === undefined ? [undefined, this.ended] : [group, undefined];
	}

	close(): void {}
}

/** Reads every catalog of track, returning the payloads applied, in order. */
async function applied(track: Track): Promise<string[]> {
	const payloads: string[] = [];
	await readCatalogs(track, new Promise(() => {}), (payload) => {
		payloads.push(new TextDecoder().decode(payload));
	});
	return payloads;
}

Deno.test("readCatalogs applies each catalog in sequence order", async () => {
	const track = new FakeTrack([
		new FakeGroup(0, "a"),
		new FakeGroup(1, "b"),
		new FakeGroup(3, "c"),
	]);

	const payloads = await applied(track);

	assertEquals(payloads, ["a", "b", "c"]);
});

Deno.test("readCatalogs skips a catalog older than one already applied", async () => {
	const stale = new FakeGroup(1, "old");
	const track = new FakeTrack([new FakeGroup(2, "new"), stale, new FakeGroup(3, "newer")]);

	const payloads = await applied(track);

	assertEquals(payloads, ["new", "newer"]);
	assertEquals(stale.cancelled, true);
});

Deno.test("readCatalogs skips a repeated sequence", async () => {
	const track = new FakeTrack([new FakeGroup(0, "a"), new FakeGroup(0, "a again")]);

	const payloads = await applied(track);

	assertEquals(payloads, ["a"]);
});

Deno.test("readCatalogs returns the error the track ended with", async () => {
	const track = new FakeTrack([new FakeGroup(0, "a")]);

	const err = await readCatalogs(track, new Promise(() => {}), () => {});

	assertEquals(err, track.ended);
});
