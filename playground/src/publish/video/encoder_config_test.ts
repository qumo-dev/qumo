// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals, assertRejects } from "@std/assert";
import {
	NoVideoCodecError,
	pickVideoConfig,
	videoCandidates,
	type VideoSupport,
} from "./encoder_config.ts";

const SETTINGS = { width: 1280, height: 720, framerate: 30, bitrate: 2_000_000 };

// A browser that supports exactly the configs `accepts` says yes to.
function supporting(accepts: (config: VideoEncoderConfig) => boolean): VideoSupport {
	return (config) => Promise.resolve({ supported: accepts(config), config });
}

Deno.test("videoCandidates asks for hardware first, then software", () => {
	const candidates = videoCandidates(SETTINGS, true);

	const order = candidates.map((c) => `${c.hardwareAcceleration ?? "software"} ${c.codec}`);
	assertEquals(order.slice(0, 2), [
		"prefer-hardware vp09.00.10.08",
		"prefer-hardware vp09",
	]);
	assertEquals(order.slice(11, 13), ["software avc1.640028", "software avc1.4D401F"]);
	assertEquals(order.length, 22);
});

Deno.test("videoCandidates asks for software only when hardware cannot be told", () => {
	const candidates = videoCandidates(SETTINGS, false);

	const asked = new Set(candidates.map((c) => c.hardwareAcceleration));
	assertEquals([...asked], [undefined]);
	assertEquals(candidates[0]?.codec, "avc1.640028");
});

Deno.test("videoCandidates scales the bitrate to what each codec needs", () => {
	const cases = [
		{ codec: "avc1.640028", want: 2_000_000 },
		{ codec: "vp09", want: 1_600_000 },
		{ codec: "av01", want: 1_200_000 },
		{ codec: "vp8", want: 2_200_000 },
		{ codec: "hev1", want: 2_000_000 },
	] as const;
	const candidates = videoCandidates(SETTINGS, false);

	for (const c of cases) {
		assertEquals(candidates.find((x) => x.codec === c.codec)?.bitrate, c.want, c.codec);
	}
});

Deno.test("videoCandidates keeps H.264 parameter sets in the stream", () => {
	const candidates = videoCandidates(SETTINGS, false);

	assertEquals(candidates.find((c) => c.codec === "avc1")?.avc, { format: "annexb" });
	assertEquals(candidates.find((c) => c.codec === "vp8")?.avc, undefined);
});

Deno.test("pickVideoConfig takes the first candidate the browser supports", async () => {
	const candidates = videoCandidates(SETTINGS, true);

	const picked = await pickVideoConfig(
		candidates,
		supporting((c) => c.codec === "vp8" || c.codec === "avc1.42E01E"),
	);

	assertEquals(picked.codec, "avc1.42E01E");
	assertEquals(picked.hardwareAcceleration, "prefer-hardware");
});

Deno.test("pickVideoConfig falls back to software when no hardware encoder fits", async () => {
	const candidates = videoCandidates(SETTINGS, true);

	const picked = await pickVideoConfig(
		candidates,
		supporting((c) => c.hardwareAcceleration === undefined && c.codec === "vp8"),
	);

	assertEquals(picked.codec, "vp8");
	assertEquals(picked.hardwareAcceleration, undefined);
});

Deno.test("pickVideoConfig rejects with NoVideoCodecError when nothing is supported", async () => {
	const candidates = videoCandidates(SETTINGS, true);

	await assertRejects(
		() => pickVideoConfig(candidates, supporting(() => false)),
		NoVideoCodecError,
		"no supported video codec",
	);
});
