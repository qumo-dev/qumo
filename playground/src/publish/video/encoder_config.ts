// Picks the video encoder: the first codec, in order of preference, that this
// browser says it can encode.

/** What the picture to encode is, and how much it may cost. */
export interface VideoSettings {
	/** The picture's size, in pixels. */
	width: number;
	height: number;
	/** Frames per second. */
	framerate: number;
	/** Bits per second to aim for. */
	bitrate: number;
}

/** No codec this browser can encode fits the settings. */
export class NoVideoCodecError extends Error {
	constructor() {
		super("no supported video codec");
		this.name = "NoVideoCodecError";
	}
}

// With an encoder in hardware, the newer codecs cost nothing extra and give a
// better picture for the bitrate.
const HARDWARE_CODECS = [
	"vp09.00.10.08",
	"vp09",
	"avc1.640028",
	"avc1.4D401F",
	"avc1.42E01E",
	"avc1",
	"av01.0.08M.08",
	"av01",
	"hev1.1.6.L93.B0",
	"hev1",
	"vp8",
] as const;

// In software, H.264 and VP8 are the ones cheap enough to encode live.
const SOFTWARE_CODECS = [
	"avc1.640028",
	"avc1.4D401F",
	"avc1.42E01E",
	"avc1",
	"vp8",
	"vp09.00.10.08",
	"vp09",
	"hev1.1.6.L93.B0",
	"hev1",
	"av01.0.08M.08",
	"av01",
] as const;

// The bitrate asked for is what H.264 needs; the others reach the same
// picture with this share of it.
function bitrateShare(codec: string): number {
	if (codec.startsWith("vp09")) return 0.8;
	if (codec.startsWith("av01")) return 0.6;
	if (codec === "vp8") return 1.1;
	return 1;
}

/**
 * The encoder configs to try, most preferred first.
 *
 * @param hardware - Whether to ask for a hardware encoder first. A browser
 *   that cannot say whether it has one (Firefox) is asked for software only.
 */
export function videoCandidates(settings: VideoSettings, hardware: boolean): VideoEncoderConfig[] {
	const config = (codec: string, inHardware: boolean): VideoEncoderConfig => ({
		codec,
		width: settings.width,
		height: settings.height,
		framerate: settings.framerate,
		bitrate: Math.round(settings.bitrate * bitrateShare(codec)),
		latencyMode: "realtime",
		...(inHardware ? { hardwareAcceleration: "prefer-hardware" as const } : {}),
		// H.264 and H.265 carry their parameter sets in the stream, so a
		// subscriber that joins at any keyframe has them.
		...(codec.startsWith("avc1") ? { avc: { format: "annexb" as const } } : {}),
		...(codec.startsWith("hev1") ? { hevc: { format: "annexb" as const } } : {}),
	});

	return [
		...(hardware ? HARDWARE_CODECS.map((codec) => config(codec, true)) : []),
		...SOFTWARE_CODECS.map((codec) => config(codec, false)),
	];
}

/** Says whether an encoder config can be used, as `VideoEncoder.isConfigSupported` does. */
export type VideoSupport = (config: VideoEncoderConfig) => Promise<VideoEncoderSupport>;

/**
 * The first of `candidates` the browser supports, as the browser normalised it.
 *
 * @throws {NoVideoCodecError} If it supports none of them.
 */
export async function pickVideoConfig(
	candidates: readonly VideoEncoderConfig[],
	supports: VideoSupport = (config) => VideoEncoder.isConfigSupported(config),
): Promise<VideoEncoderConfig> {
	for (const candidate of candidates) {
		const { supported, config } = await supports(candidate);
		if (supported && config !== undefined) return config;
	}
	throw new NoVideoCodecError();
}
