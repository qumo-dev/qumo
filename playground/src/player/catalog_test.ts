// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { readCatalogs, videoRenditions } from "./catalog.ts";
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

// An MSF catalog payload as qumo abr publishes one: the source video and two
// renditions in alternate group 1, and audio outside it.
function abrCatalog(extra: Record<string, unknown>[] = []): Uint8Array {
	const video = (name: string, height: number, bitrate?: number) => ({
		name,
		packaging: "loc",
		role: "video",
		isLive: true,
		codec: "avc1.64001f",
		width: Math.round(height * 16 / 9),
		height,
		altGroup: 1,
		initRef: name,
		...(bitrate ? { bitrate } : {}),
	});
	return new TextEncoder().encode(JSON.stringify({
		version: 1,
		tracks: [
			video("video-360p", 360, 800_000),
			video("video", 1080),
			{
				name: "audio",
				packaging: "loc",
				role: "audio",
				isLive: true,
				codec: "mp4a.40.2",
				samplerate: 48000,
			},
			video("video-720p", 720, 2_500_000),
			...extra,
		],
		initDataList: ["video", "video-720p", "video-360p"].map((id) => ({
			id,
			type: "inline",
			data: btoa(id),
		})),
	}));
}

Deno.test("videoRenditions lists the alternates of the first video track, tallest first", () => {
	const got = videoRenditions(abrCatalog());

	assertEquals(got.map((r) => [r.track, r.label, r.bitrate]), [
		["video", "1080p", undefined],
		["video-720p", "720p", 2_500_000],
		["video-360p", "360p", 800_000],
	]);
	const description = got[1].config.description as ArrayBuffer;
	assertEquals(new TextDecoder().decode(description), "video-720p");
	assertEquals(got[1].config.codedHeight, 720);
});

Deno.test("videoRenditions leaves out video tracks of another alternate group", () => {
	const got = videoRenditions(abrCatalog([{
		name: "slides",
		packaging: "loc",
		role: "video",
		codec: "avc1.64001f",
		height: 1080,
		altGroup: 2,
	}]));

	assertEquals(got.map((r) => r.track), ["video", "video-720p", "video-360p"]);
});

Deno.test("videoRenditions of a catalog without alternates is its first video track", () => {
	const payload = new TextEncoder().encode(JSON.stringify({
		version: 1,
		tracks: [
			{
				name: "video",
				packaging: "loc",
				role: "video",
				codec: "avc1.42e01f",
				width: 640,
				height: 360,
			},
			{ name: "video2", packaging: "loc", role: "video", codec: "avc1.42e01f", height: 720 },
		],
	}));

	assertEquals(videoRenditions(payload).map((r) => r.track), ["video"]);
});

Deno.test("videoRenditions of a catalog with no video is empty", () => {
	const payload = new TextEncoder().encode(JSON.stringify({
		version: 1,
		tracks: [{ name: "audio", packaging: "loc", role: "audio", codec: "opus" }],
	}));

	assertEquals(videoRenditions(payload), []);
});
