import { parseCatalog } from "@qumo/moq/msf";

/** Decoder configurations carried by one catalog payload. */
export interface DecoderConfigs {
	video?: VideoDecoderConfig;
	audio?: AudioDecoderConfig;
}

/** Reads the video and audio decoder configurations out of an MSF catalog payload. */
export function decoderConfigs(payload: Uint8Array): DecoderConfigs {
	const catalog = parseCatalog(payload);
	const configs: DecoderConfigs = {};

	const video = catalog.tracks?.find((t) => t.role === "video");
	if (video?.codec) {
		// initData is the AVCC description for avc1.* streams.
		const description = decodeInitData(video.initData);
		configs.video = {
			codec: video.codec,
			codedWidth: video.width,
			codedHeight: video.height,
			hardwareAcceleration: "prefer-software",
			optimizeForLatency: true,
			...(description ? { description } : {}),
		};
	}

	const audio = catalog.tracks?.find((t) => t.role === "audio");
	if (audio?.codec) {
		// initData is the AudioSpecificConfig. Raw AAC frames (no ADTS, as
		// RTSP/RTMP ingest emits) need it as `description` for WebCodecs
		// AudioDecoder to parse them.
		const description = decodeInitData(audio.initData);
		configs.audio = {
			codec: audio.codec,
			sampleRate: audio.samplerate ?? 48000,
			numberOfChannels: parseInt(audio.channelConfig ?? "2", 10),
			...(description ? { description } : {}),
		};
	}

	return configs;
}

function decodeInitData(initData: string | undefined): ArrayBuffer | undefined {
	if (!initData) return undefined;
	const binary = atob(initData);
	const bytes = new Uint8Array(binary.length);
	for (let i = 0; i < binary.length; i++) {
		bytes[i] = binary.charCodeAt(i);
	}
	return bytes.buffer;
}
