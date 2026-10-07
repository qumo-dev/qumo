// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals, assertRejects } from "@std/assert";
import { NoAudioCodecError, opusConfig, pickAudioConfig } from "./encoder_config.ts";

Deno.test("opusConfig asks for Opus at the given rate and channel count", () => {
	const config = opusConfig(48000, 2);

	assertEquals({ ...config, opus: undefined }, {
		codec: "opus",
		sampleRate: 48000,
		numberOfChannels: 2,
		bitrate: 64_000,
		bitrateMode: "variable",
		opus: undefined,
	});
});

Deno.test("opusConfig tunes one channel for a voice and two for music", () => {
	const cases = [
		{ channels: 1, want: { application: "voip", signal: "voice" } },
		{ channels: 2, want: { application: "audio", signal: "music" } },
	] as const;

	for (const c of cases) {
		const tuning: unknown = opusConfig(48000, c.channels).opus;

		assertEquals(tuning, c.want);
	}
});

Deno.test("pickAudioConfig returns the config as the browser normalised it", async () => {
	const normalised = { ...opusConfig(48000, 2), bitrate: 63_000 };

	const picked = await pickAudioConfig(
		48000,
		2,
		() => Promise.resolve({ supported: true, config: normalised }),
	);

	assertEquals(picked, normalised);
});

Deno.test("pickAudioConfig rejects with NoAudioCodecError when Opus is not supported", async () => {
	await assertRejects(
		() => pickAudioConfig(48000, 2, () => Promise.resolve({ supported: false })),
		NoAudioCodecError,
		"no supported audio codec",
	);
});
