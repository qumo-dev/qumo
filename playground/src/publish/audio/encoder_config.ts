// Picks the audio encoder. Opus is the only codec every browser with
// WebCodecs can encode, and the only one the viewer and the egress expect.

/** This browser cannot encode Opus at the settings asked for. */
export class NoAudioCodecError extends Error {
	constructor() {
		super("no supported audio codec");
		this.name = "NoAudioCodecError";
	}
}

// Bits per second: enough for stereo music, which a shared tab may be.
const BITRATE = 64_000;

// What Opus is told about its input. Chrome takes both; the DOM typings do
// not list them yet.
interface OpusTuning extends OpusEncoderConfig {
	application?: "voip" | "audio" | "lowdelay";
	signal?: "auto" | "music" | "voice";
}

/** The Opus encoder config asked for, before the browser normalises it. */
export function opusConfig(sampleRate: number, channels: number): AudioEncoderConfig {
	// One channel is taken to be a voice; two may be music, as a shared tab is.
	const opus: OpusTuning = channels === 1
		? { application: "voip", signal: "voice" }
		: { application: "audio", signal: "music" };
	return {
		codec: "opus",
		sampleRate,
		numberOfChannels: channels,
		bitrate: BITRATE,
		bitrateMode: "variable",
		opus,
	};
}

/** Says whether an encoder config can be used, as `AudioEncoder.isConfigSupported` does. */
export type AudioSupport = (config: AudioEncoderConfig) => Promise<AudioEncoderSupport>;

/**
 * The Opus config for `sampleRate` and `channels`, as the browser normalised it.
 *
 * @throws {NoAudioCodecError} If the browser does not support it.
 */
export async function pickAudioConfig(
	sampleRate: number,
	channels: number,
	supports: AudioSupport = (config) => AudioEncoder.isConfigSupported(config),
): Promise<AudioEncoderConfig> {
	const { supported, config } = await supports(opusConfig(sampleRate, channels));
	if (supported && config !== undefined) return config;
	throw new NoAudioCodecError();
}
