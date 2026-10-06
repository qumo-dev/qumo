import { createLogger } from "@okdaichi/media-log";
import { patternStream } from "./pattern.ts";

const log = createLogger("media");

export type MediaSourceType = "camera" | "screen" | "pattern";

export interface MediaConstraints {
	width?: number;
	height?: number;
	frameRate?: number;
}

export const getMediaStream = async (
	type: MediaSourceType,
	constraints?: MediaConstraints,
): Promise<MediaStream> => {
	// Apply caller-provided constraints as `ideal` hints. Defaults match the
	// pre-control behavior (720p@30 camera, 1080p@30 screen).
	const w = constraints?.width;
	const h = constraints?.height;
	const fps = constraints?.frameRate;
	try {
		switch (type) {
			case "camera":
				return await navigator.mediaDevices.getUserMedia({
					video: {
						width: { ideal: w ?? 1280 },
						height: { ideal: h ?? 720 },
						frameRate: { ideal: fps ?? 30 },
					},
					audio: true,
				});
			case "screen":
				return await navigator.mediaDevices.getDisplayMedia({
					video: {
						width: { ideal: w ?? 1920 },
						height: { ideal: h ?? 1080 },
						frameRate: { ideal: fps ?? 30 },
					},
					audio: true,
				});
			case "pattern":
				// Drawn here, so it is exactly what was asked for and needs
				// no permission.
				return patternStream({
					width: w ?? 1280,
					height: h ?? 720,
					frameRate: fps ?? 30,
				});
			default:
				throw new Error(`Unsupported media source type: ${type}`);
		}
	} catch (err) {
		// Rethrow the ORIGINAL error (typically a DOMException) unchanged.
		// The UI classifies these by name (NotAllowedError, NotFoundError, …)
		// into an actionable message, so wrapping them in a plain Error — which
		// drops .name — would defeat that. We only log the source type here.
		log.error(`get${type} stream failed`, { err });
		throw err;
	}
};
