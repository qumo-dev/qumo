import { type Catalog, parseCatalog, type Track as CatalogTrack } from "@qumo/moq/msf";
import type { Track } from "./source.ts";

/**
 * Reads catalog groups from track and hands each catalog payload to apply,
 * until the track ends or `done` resolves; returns the error acceptGroup
 * ended with. The publisher sends the whole catalog again on every change,
 * each group on its own stream, so an older catalog can arrive after a newer
 * one: it is skipped, and the catalog applied last is always the newest seen.
 */
export async function readCatalogs(
	track: Track,
	done: Promise<void>,
	apply: (payload: Uint8Array) => void,
): Promise<Error> {
	let newest: number | undefined;
	for (;;) {
		const [group, err] = await track.acceptGroup(done);
		if (group === undefined) return err;
		if (newest !== undefined && group.sequence <= newest) {
			group.cancel();
			continue;
		}
		newest = group.sequence;
		for await (const frame of group.frames()) {
			apply(frame.bytes);
		}
	}
}

/** One video track a player can choose to decode, of several alternatives. */
export interface Rendition {
	/** The track's name, to subscribe to. */
	readonly track: string;
	/** What to call it, such as "720p". */
	readonly label: string;
	/** The track's bitrate in bits per second, when the catalog states it. */
	readonly bitrate: number | undefined;
	readonly height: number | undefined;
	readonly config: VideoDecoderConfig;
}

/**
 * Reads the video renditions out of an MSF catalog payload: the first video
 * track, and every other video track in its alternate group (draft-ietf-moq-msf
 * `altGroup`), tallest first. A catalog with one video track, or one whose
 * first video track is in no alternate group, has one rendition; one with no
 * decodable video track has none.
 */
export function videoRenditions(payload: Uint8Array): Rendition[] {
	return renditionsOf(parseCatalog(payload));
}

function renditionsOf(catalog: Catalog): Rendition[] {
	const tracks = catalog.tracks ?? [];
	const first = tracks.find(decodableVideo);
	if (first === undefined) return [];
	const alternates = first.altGroup === undefined
		? [first]
		: tracks.filter(decodableVideo).filter((t) => t.altGroup === first.altGroup);
	return alternates
		.map(rendition)
		.sort((a, b) => (b.height ?? 0) - (a.height ?? 0) || (b.bitrate ?? 0) - (a.bitrate ?? 0));
}

type DecodableTrack = CatalogTrack & { name: string; codec: string };

function decodableVideo(t: CatalogTrack): t is DecodableTrack {
	return t.role === "video" && !!t.name && !!t.codec;
}

function rendition(t: DecodableTrack): Rendition {
	return {
		track: t.name,
		label: t.height ? `${t.height}p` : (t.label ?? t.name),
		bitrate: t.bitrate,
		height: t.height,
		config: videoConfig(t),
	};
}

function videoConfig(t: CatalogTrack & { codec: string }): VideoDecoderConfig {
	// initData is the AVCC description for avc1.* streams.
	const description = decodeInitData(t.initData);
	return {
		codec: t.codec,
		codedWidth: t.width,
		codedHeight: t.height,
		hardwareAcceleration: "prefer-software",
		optimizeForLatency: true,
		...(description ? { description } : {}),
	};
}

/**
 * Reads the audio decoder configuration of the first audio track out of an
 * MSF catalog payload, or undefined when it has none. Video is configured per
 * rendition (videoRenditions).
 */
export function audioConfig(payload: Uint8Array): AudioDecoderConfig | undefined {
	const audio = parseCatalog(payload).tracks?.find((t) => t.role === "audio");
	if (!audio?.codec) return undefined;
	// initData is the AudioSpecificConfig. Raw AAC frames (no ADTS, as
	// RTSP/RTMP ingest emits) need it as `description` for WebCodecs
	// AudioDecoder to parse them.
	const description = decodeInitData(audio.initData);
	return {
		codec: audio.codec,
		sampleRate: audio.samplerate ?? 48000,
		numberOfChannels: parseInt(audio.channelConfig ?? "2", 10),
		...(description ? { description } : {}),
	};
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
