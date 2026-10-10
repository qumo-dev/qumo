// Scenario registry for the demo. One source of truth for each pipeline's UI
// mode, for the WebTransport port of the ones that are ingests (the relay's
// own port comes from the runtime config), and for the push scheme and port
// used to build the ffmpeg command shown in the UI.

import type { RelayEndpoint } from "./config.ts";

export type ScenarioId = "echo" | "rtmp" | "rtsp" | "camera" | "hls";
export type ScenarioMode = "publish-subscribe" | "subscribe";

export interface Scenario {
	id: ScenarioId;
	label: string;
	/** One-line description shown below the scenario picker. */
	description: string;
	/**
	 * The port of this scenario's WebTransport origin, when it is an ingest
	 * with a port of its own. Absent for the scenarios served by the relay
	 * itself, whose port comes from the runtime config.
	 */
	port?: number;
	mode: ScenarioMode;
	/** Ingest-only: scheme + port an external encoder pushes to. */
	pushScheme?: "rtmp" | "rtsp";
	pushPort?: number;
}

export const SCENARIOS: Record<ScenarioId, Scenario> = {
	echo: {
		id: "echo",
		label: "Webcam",
		description:
			"Publish from your camera or screen, and subscribe back — full MoQ round-trip in the browser.",
		mode: "publish-subscribe",
	},
	camera: {
		id: "camera",
		label: "IP Camera",
		description:
			"Pull a live RTSP stream from an IP camera (e.g. rtsp://user:pass@192.168.1.100/stream) directly into MoQ.",
		port: 4543,
		mode: "subscribe",
	},
	rtmp: {
		id: "rtmp",
		label: "RTMP",
		description: "Push an RTMP stream from ffmpeg and subscribe in the browser.",
		port: 4443,
		mode: "subscribe",
		pushScheme: "rtmp",
		pushPort: 1935,
	},
	rtsp: {
		id: "rtsp",
		label: "RTSP Push",
		description: "Push an RTSP stream from ffmpeg and subscribe in the browser.",
		port: 4543,
		mode: "subscribe",
		pushScheme: "rtsp",
		pushPort: 8554,
	},
	hls: {
		id: "hls",
		label: "HLS",
		description: "Publish from your camera over MoQ and play it back through the HLS egress.",
		mode: "publish-subscribe",
	},
};

export const SCENARIO_ORDER: ScenarioId[] = ["echo", "camera", "rtmp", "rtsp", "hls"];

export function isScenarioId(x: string): x is ScenarioId {
	return x in SCENARIOS;
}

// Each scenario is a WebTransport origin on the one host the runtime config
// names (see relayEndpoint in config.ts): the relay itself, on the port it was
// started on, or an ingest on a port of its own.
// Whether a scenario's session is with the relay itself, not with an ingest
// on a port of its own. Only the relay takes WebSocket (see transport.ts).
export function servedByRelay(id: ScenarioId): boolean {
	return SCENARIOS[id].port === undefined;
}

export function relayUrlFor(id: ScenarioId, relay: RelayEndpoint): string {
	return `https://${relay.host}:${SCENARIOS[id].port ?? relay.port}`;
}

// ffmpeg source pipeline shared by the RTMP/RTSP push instructions.
const FFMPEG_PIPELINE =
	"ffmpeg -re -f lavfi -i testsrc2=size=1280x720:rate=30 -f lavfi -i sine=frequency=440:sample_rate=48000 " +
	"-c:v libx264 -preset veryfast -tune zerolatency -profile:v baseline -g 60 -c:a aac -ar 48000 -ac 2";

// The push target URL for an ingest scenario, embedding the (unique) path so an
// external encoder and the subscriber always agree on the stream.
export function pushTargetFor(id: ScenarioId, path: string, host: string): string {
	const s = SCENARIOS[id];
	if (!s.pushScheme || !s.pushPort) return "";
	return `${s.pushScheme}://${host}:${s.pushPort}${path}`;
}

// Full copy-pasteable ffmpeg command that pushes to the given path.
export function pushCommandFor(id: ScenarioId, path: string, host: string): string {
	const s = SCENARIOS[id];
	if (!s.pushScheme) return "";
	const out = s.pushScheme === "rtmp" ? "-f flv" : "-f rtsp -rtsp_transport tcp";
	const target = pushTargetFor(id, path, host);
	return `${FFMPEG_PIPELINE} ${out} ${target}`;
}
