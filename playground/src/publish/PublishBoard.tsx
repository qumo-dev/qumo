import {
	type Accessor,
	type Component,
	createMemo,
	createSignal,
	For,
	onCleanup,
	onMount,
	Show,
} from "solid-js";
import type { TrackMux } from "@qumo/moq";
import { createLogger } from "@okdaichi/media-log";
import { Camera, Monitor, Tv } from "lucide-solid";
import { PreviewCanvas } from "../components/PreviewCanvas.tsx";
import { type Stat, StatsOverlay } from "../components/StatsOverlay.tsx";
import type { Recorder } from "../devtools/recorder.ts";
import { friendlyMessage } from "../errors.ts";
import { createStatsTicker } from "../stats.ts";
import { getMediaStream, type MediaSourceType } from "./media.ts";
import { Publisher, VideoFailedError } from "./publisher.ts";

const log = createLogger("publish");

// Encode-quality presets (#135). Resolution maps to getUserMedia `ideal`
// constraints (the camera picks the nearest mode); the encoder then encodes at
// the actual captured dimensions. Bitrate/framerate go straight to the encoder.
const RESOLUTIONS = {
	"480p": { width: 854, height: 480 },
	"720p": { width: 1280, height: 720 },
	"1080p": { width: 1920, height: 1080 },
} as const;
type Resolution = keyof typeof RESOLUTIONS;
const FRAMERATES = [24, 30, 60] as const;
type Framerate = (typeof FRAMERATES)[number];

// What a <select> hands back is a string; these say whether it is one of the picks.
function isResolution(value: string): value is Resolution {
	return Object.hasOwn(RESOLUTIONS, value);
}

// What to tell the user about a failed run. The message is chosen by what
// went wrong underneath, which a VideoFailedError carries as its cause: an
// encoder that rejects a codec says so by name.
function failureMessage(err: unknown): string {
	return friendlyMessage(err instanceof VideoFailedError ? err.cause : err);
}

function isFramerate(value: number): value is Framerate {
	return FRAMERATES.some((framerate) => framerate === value);
}
const BITRATE_MIN = 500_000;
const BITRATE_MAX = 6_000_000;
const BITRATE_STEP = 100_000;

// Media-source options for the segmented switcher. `label` is also used for the
// "Streaming from" status line so it doesn't show the raw signal value. Icons
// come from lucide-solid (public icon set) rather than hand-rolled SVG.
const SOURCES: { id: MediaSourceType; label: string; icon: Component<{ class?: string }> }[] = [
	{ id: "camera", label: "Camera", icon: Camera },
	{ id: "screen", label: "Screen", icon: Monitor },
	// A moving picture and a tone made in the page: no camera, no permission.
	{ id: "pattern", label: "Test pattern", icon: Tv },
];

export function PublishBoard(
	props: { mux: TrackMux; path: Accessor<string>; recorder?: Recorder },
) {
	const mux = props.mux;

	const [sourceType, setSourceType] = createSignal<MediaSourceType>("camera");
	const [isStreaming, setIsStreaming] = createSignal(false);
	const [error, setError] = createSignal<string | null>(null);
	const [canvasWidth, setCanvasWidth] = createSignal(1280);
	const [canvasHeight, setCanvasHeight] = createSignal(720);
	// Encode-quality controls (applied at Start; stop+restart to change mid-session).
	const [resolution, setResolution] = createSignal<Resolution>("720p");
	const [framerate, setFramerate] = createSignal<Framerate>(30);
	const [bitrate, setBitrate] = createSignal(2_500_000);

	// Live stats overlay (#139): fps + media bitrate from a 1s rolling meter, plus
	// the encoder's queue depth sampled on the same tick. Cleared on stop.
	const [encQueue, setEncQueue] = createSignal(0);
	const videoStats = createStatsTicker(
		1000,
		() => setEncQueue(publisher?.encodeQueueSize ?? 0),
	);
	onCleanup(() => videoStats.stop());

	let canvasEle: HTMLCanvasElement | undefined;
	// Captures, encodes and sends; made once the canvas it draws on exists.
	let publisher: Publisher | undefined;
	// Set on unmount. Starting waits on the browser's permission prompt, and
	// a scenario switch can unmount the board before the user has answered.
	let disposed = false;

	const stopped = () => {
		videoStats.stop();
		setIsStreaming(false);
	};

	onMount(() => {
		if (canvasEle === undefined) return;
		publisher = new Publisher({ mux, canvas: canvasEle, observer: props.recorder });
		publisher.onencoded = (bytes) => videoStats.mark(bytes);
		// The run ended by itself: sharing was stopped from the browser's
		// own controls, or the camera was unplugged.
		publisher.onended = stopped;
		publisher.onerror = (err) => {
			setError(failureMessage(err));
			stopped();
		};
	});

	const startStreaming = async () => {
		if (publisher === undefined) {
			setError("The preview is not ready yet.");
			return;
		}
		setError(null);

		// The chosen resolution and frame rate are what the source is asked
		// for; it delivers the nearest it has, and that is what is encoded.
		const target = RESOLUTIONS[resolution()];
		let stream: MediaStream;
		try {
			stream = await getMediaStream(sourceType(), {
				width: target?.width,
				height: target?.height,
				frameRate: framerate(),
			});
		} catch (err) {
			// Pass the source type so a denied screen-share reads as screen-share,
			// not "Camera or microphone".
			setError(friendlyMessage(err, sourceType()));
			log.error("failed to get media", { err });
			return;
		}
		if (disposed) {
			for (const track of stream.getTracks()) track.stop();
			return;
		}

		try {
			const picture = await publisher.start(stream, {
				path: props.path(),
				framerate: framerate(),
				bitrate: bitrate(),
			});
			// Stopped before it began.
			if (picture === undefined) return;

			// Size the preview canvas to the picture actually captured.
			setCanvasWidth(picture.width);
			setCanvasHeight(picture.height);
			setIsStreaming(true);
			videoStats.start();
			log.info("started streaming", { source: sourceType() });
		} catch (err) {
			setError(failureMessage(err));
			log.error("failed to start streaming", { err });
		}
	};

	const stopStreaming = () => {
		publisher?.stop();
		stopped();
	};

	// Scenario switches remount <ScenarioView>, so without this every visit
	// would leave an AudioContext and its worklet behind.
	onCleanup(() => {
		disposed = true;
		stopStreaming();
		publisher?.close();
		publisher = undefined;
	});

	const sourceLabel = createMemo(() =>
		SOURCES.find((s) => s.id === sourceType())?.label ?? sourceType()
	);
	const stats = createMemo((): Stat[] => [
		{ label: "res", value: `${canvasWidth()}×${canvasHeight()}` },
		{ label: "fps", value: videoStats.stats().fps },
		{ label: "br", value: `${videoStats.stats().bitrateMbps} Mbps` },
		...(encQueue() > 0 ? [{ label: "queue", value: encQueue() }] : []),
	]);

	return (
		<div class="publish-board">
			<h2>Publish Board</h2>

			<div class="controls">
				<div class="source-selector">
					<label>Media Source</label>
					<div class="segmented" role="group" aria-label="Media source">
						<For each={SOURCES}>
							{(s) => (
								<button
									type="button"
									class="segmented-btn"
									classList={{ active: sourceType() === s.id }}
									aria-pressed={sourceType() === s.id}
									disabled={isStreaming()}
									onClick={() => setSourceType(s.id)}
									title={isStreaming()
										? "Stop streaming to switch source"
										: s.label}
								>
									<s.icon class="source-icon" /> {s.label}
								</button>
							)}
						</For>
					</div>
				</div>

				<div class="encoder-controls">
					<label>
						Quality
						<select
							value={resolution()}
							onChange={(e) => {
								const picked = e.currentTarget.value;
								if (isResolution(picked)) setResolution(picked);
							}}
							disabled={isStreaming()}
						>
							<For each={Object.keys(RESOLUTIONS)}>
								{(id) => <option value={id}>{id}</option>}
							</For>
						</select>
					</label>
					<label>
						FPS
						<select
							value={framerate()}
							onChange={(e) => {
								const picked = Number(e.currentTarget.value);
								if (isFramerate(picked)) setFramerate(picked);
							}}
							disabled={isStreaming()}
						>
							<For each={FRAMERATES}>
								{(fps) => <option value={fps}>{fps}</option>}
							</For>
						</select>
					</label>
					<label class="bitrate-control">
						Bitrate
						<input
							type="range"
							min={BITRATE_MIN}
							max={BITRATE_MAX}
							step={BITRATE_STEP}
							value={bitrate()}
							onInput={(e) => setBitrate(Number(e.currentTarget.value))}
							disabled={isStreaming()}
						/>
						<span class="bitrate-value">{(bitrate() / 1_000_000).toFixed(1)} Mbps</span>
					</label>
				</div>

				<div class="stream-controls">
					<Show
						when={!isStreaming()}
						fallback={
							<button type="button" onClick={stopStreaming} class="btn-stop">
								Stop Streaming
							</button>
						}
					>
						<button type="button" onClick={startStreaming} class="btn-start">
							Start Streaming
						</button>
					</Show>
				</div>
			</div>

			<Show when={error()}>
				<div class="error-message">{error()}</div>
			</Show>

			<Show when={isStreaming()}>
				<div class="status-message">Streaming from: {sourceLabel()}</div>
			</Show>

			<div class="video-preview">
				<PreviewCanvas
					ref={(canvas) => {
						canvasEle = canvas;
					}}
					width={canvasWidth()}
					height={canvasHeight()}
				/>
				<Show when={isStreaming()}>
					<StatsOverlay stats={stats()} />
				</Show>
			</div>
		</div>
	);
}
