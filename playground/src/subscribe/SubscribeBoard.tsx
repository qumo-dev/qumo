import { type Accessor, createEffect, createSignal, onCleanup, onMount, Show } from "solid-js";
import type { Session } from "@qumo/moq";
import { deserializeMediaFrame } from "../publish/media_frame.ts";
import { friendlyMessage } from "../errors.ts";
import { createLogger, createMediaLogger, MediaTags } from "@okdaichi/media-log";
import { moqSource } from "../player/moq_source.ts";
import { type PlaybackObserver, Viewer } from "../player/viewer.ts";
import { createStatsTicker } from "../stats.ts";

// Tagged, structured logging via @okdaichi/media-log. The video (decoder) logger
// also carries media meters (fps/bitrate/gauge) that flush one diagnostic line
// per second alongside the UI overlay.
const log = createLogger("subscribe");
const videoLog = createMediaLogger(MediaTags.decoder);
const decFps = videoLog.meter.fps("decode");
const decBitrate = videoLog.meter.bitrate("ingress");
const rttGauge = videoLog.meter.gauge("rtt", { unit: "ms" });
const queueGauge = videoLog.meter.gauge("decode queue");

export function SubscribeBoard(
	props: { session: Promise<Session>; path: Accessor<string>; observer?: PlaybackObserver },
) {
	const [isSubscribed, setIsSubscribed] = createSignal(false);
	const [error, setError] = createSignal<string | null>(null);
	const [canvasWidth, setCanvasWidth] = createSignal(1280);
	const [canvasHeight, setCanvasHeight] = createSignal(720);
	// Viewer controls (#136): pure client-side, no timeline (MoQ is live-only).
	const [volume, setVolume] = createSignal(1);
	const [muted, setMuted] = createSignal(false);
	const [isFullscreen, setIsFullscreen] = createSignal(false);

	// Live stats overlay (#139): fps + media bitrate from a 1s rolling meter,
	// plus decoder queue depth and the session RTT sampled on the same tick.
	const [decQueue, setDecQueue] = createSignal(0);
	const [rtt, setRtt] = createSignal(0);
	let statsSession: Session | undefined;
	const videoStats = createStatsTicker(1000, () => {
		const q = viewer?.decodeQueueSize ?? 0;
		setDecQueue(q);
		queueGauge.sample(q);
		if (statsSession) {
			void statsSession.getStats().then((s) => {
				const r = s.rtt ?? 0;
				setRtt(r);
				rttGauge.sample(r);
			});
		}
	});

	let canvasEle: HTMLCanvasElement | undefined;
	let previewEle: HTMLDivElement | undefined;
	let viewer: Viewer | undefined;

	// Volume runs entirely client-side — no effect on the live MoQ stream. The
	// effective level is 0 when muted, otherwise the chosen volume.
	const gainValue: Accessor<number> = () => (muted() ? 0 : volume());

	createEffect(() => {
		// Read gainValue() FIRST so the effect subscribes to muted/volume even
		// when the viewer isn't set yet on the first run — otherwise the
		// early return would track nothing and the effect would never re-run,
		// leaving mute/volume non-functional.
		const g = gainValue();
		if (viewer) viewer.volume = g;
	});

	// Unmuting when volume has been dragged to 0 would otherwise leave the
	// player silent with the unmuted icon — restore a default level.
	const toggleMute = () => {
		if (muted()) {
			setMuted(false);
			if (volume() <= 0) setVolume(1);
		} else {
			setMuted(true);
		}
	};

	const toggleFullscreen = () => {
		const el = previewEle;
		if (!el) return;
		if (document.fullscreenElement === el) {
			void document.exitFullscreen();
		} else {
			void el.requestFullscreen?.();
		}
	};

	const onFullscreenChange = () => setIsFullscreen(document.fullscreenElement === previewEle);

	onMount(() => {
		if (canvasEle) {
			viewer = new Viewer({
				canvas: canvasEle,
				unpack: deserializeMediaFrame,
				observer: props.observer,
				onVideoConfig(config) {
					// Update canvas bitmap dimensions to match the actual video so portrait
					// (or any non-default-aspect) streams are not stretched.
					if (config.codedWidth) setCanvasWidth(config.codedWidth);
					if (config.codedHeight) setCanvasHeight(config.codedHeight);
				},
				onVideoChunk(byteLength) {
					// Tally the frame for the live stats overlay…
					videoStats.mark(byteLength);
					// …and for the periodic diagnostic log (fps + bitrate).
					decFps.mark();
					decBitrate.mark(byteLength);
				},
				onFail(err) {
					// friendlyMessage maps the common "nobody is publishing to this
					// path yet" case to actionable text; dropping back to Start lets
					// the user retry without clicking Stop first.
					setError(friendlyMessage(err));
					stopSubscribing();
				},
			});
			// Apply the initial level (the gain effect above only fires on changes).
			viewer.volume = gainValue();
		}
		document.addEventListener("fullscreenchange", onFullscreenChange);
	});

	onCleanup(() => {
		document.removeEventListener("fullscreenchange", onFullscreenChange);
		stopSubscribing();
		viewer?.close();
	});

	const startSubscribing = async () => {
		try {
			setError(null);

			if (!viewer) {
				throw new Error("Video context not initialized");
			}

			const session = await props.session;
			statsSession = session; // for RTT sampling in the stats ticker
			viewer.start(moqSource(session), props.path()); // path is snapshotted at start

			setIsSubscribed(true);
			videoStats.start();
		} catch (err) {
			setError(friendlyMessage(err));
			log.error("failed to start subscribing", { err });
			setIsSubscribed(false);
		}
	};

	const stopSubscribing = () => {
		viewer?.stop();
		statsSession = undefined;
		videoStats.stop();
		setIsSubscribed(false);
	};

	return (
		<div class="subscribe-board">
			<h2>Subscribe Board</h2>

			<div class="controls">
				<div class="stream-controls">
					<Show
						when={!isSubscribed()}
						fallback={
							<button type="button" onClick={stopSubscribing} class="btn-stop">
								Stop Subscribing
							</button>
						}
					>
						<button type="button" onClick={startSubscribing} class="btn-start">
							Start Subscribing
						</button>
					</Show>
				</div>
			</div>

			<Show when={error()}>
				<div class="error-message">{error()}</div>
			</Show>

			<Show when={isSubscribed()}>
				<div class="status-message">
					Subscribing to: {props.path()}
				</div>
			</Show>

			<div class="video-preview" ref={previewEle}>
				<canvas
					ref={canvasEle}
					width={canvasWidth()}
					height={canvasHeight()}
					style={{
						display: "block",
						width: "100%",
						"max-width": `${canvasWidth()}px`,
						"aspect-ratio": `${canvasWidth()} / ${canvasHeight()}`,
						border: "1px solid #ccc",
						"border-radius": "8px",
						background: "#000",
					}}
				/>
				<Show when={isSubscribed()}>
					<dl class="stats-overlay" aria-live="off">
						<div>
							<dt>res</dt>
							<dd>{canvasWidth()}×{canvasHeight()}</dd>
						</div>
						<div>
							<dt>fps</dt>
							<dd>{videoStats.stats().fps}</dd>
						</div>
						<div>
							<dt>br</dt>
							<dd>{videoStats.stats().bitrateMbps} Mbps</dd>
						</div>
						<Show when={rtt() > 0}>
							<div>
								<dt>rtt</dt>
								<dd>{rtt()} ms</dd>
							</div>
						</Show>
						<Show when={decQueue() > 0}>
							<div>
								<dt>queue</dt>
								<dd>{decQueue()}</dd>
							</div>
						</Show>
					</dl>
				</Show>
			</div>

			<div class="viewer-controls">
				<button
					type="button"
					class="copy-btn"
					onClick={toggleMute}
					aria-pressed={muted()}
					title={muted() ? "Unmute" : "Mute"}
				>
					{muted() ? "🔇" : "🔊"}
				</button>
				<input
					type="range"
					class="volume-slider"
					min={0}
					max={1}
					step={0.01}
					value={gainValue()}
					onInput={(e) => {
						const v = Number(e.currentTarget.value);
						setVolume(v);
						setMuted(v === 0);
					}}
					aria-label="Volume"
				/>
				<button
					type="button"
					class="copy-btn"
					onClick={toggleFullscreen}
					title={isFullscreen() ? "Exit fullscreen" : "Fullscreen"}
				>
					{isFullscreen() ? "⤢" : "⛶"}
				</button>
			</div>
		</div>
	);
}
