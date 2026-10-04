import { type Accessor, createSignal, onCleanup, onMount, Show } from "solid-js";
import { type BroadcastPath, type GroupWriter, TrackMux, type TrackWriter } from "@qumo/moq";
import { Broadcast, type Track } from "@qumo/moq/msf";
import {
	AudioEncodeNode,
	audioEncoderConfig,
	MediaStreamVideoSourceNode,
	VideoContext,
	VideoEncodeNode,
	videoEncoderConfig,
} from "@okdaichi/av-nodes";
import { getMediaStream, type MediaSourceType } from "./media.ts";
import { background, type CancelFunc, type Context, withCancel } from "@okdaichi/golikejs/context";
import { MediaFrame } from "./media_frame.ts";
import type { Recorder } from "../devtools/recorder.ts";
import { friendlyMessage } from "../errors.ts";
import { createMediaLogger, MediaTags } from "@okdaichi/media-log";
import { createStatsTicker } from "../stats.ts";
import { Camera, Monitor } from "lucide-solid";
import type { Component } from "solid-js";

// Encode an ArrayBuffer or ArrayBufferView as a Base64 string.
function encodeBase64(buf: ArrayBufferLike | ArrayBufferView): string {
	const bytes = ArrayBuffer.isView(buf)
		? new Uint8Array(buf.buffer as ArrayBuffer, buf.byteOffset, buf.byteLength)
		: new Uint8Array(buf as ArrayBuffer);
	let binary = "";
	for (const b of bytes) binary += String.fromCharCode(b);
	return btoa(binary);
}

const GOP_DURATION = 1000; // 1 second

// Tagged, structured, level-filtered logging via @okdaichi/media-log. The meters
// flush one diagnostic fps/bitrate line per second alongside the UI overlay.
const log = createMediaLogger(MediaTags.encoder);
const encFps = log.meter.fps("encode");
const encBitrate = log.meter.bitrate("egress");

// Encode-quality presets (#135). Resolution maps to getUserMedia `ideal`
// constraints (the camera picks the nearest mode); the encoder then encodes at
// the actual captured dimensions. Bitrate/framerate go straight to the encoder.
const RESOLUTIONS: Record<string, { width: number; height: number }> = {
	"480p": { width: 854, height: 480 },
	"720p": { width: 1280, height: 720 },
	"1080p": { width: 1920, height: 1080 },
};
const FRAMERATES = [24, 30, 60] as const;
const BITRATE_MIN = 500_000;
const BITRATE_MAX = 6_000_000;
const BITRATE_STEP = 100_000;

// Media-source options for the segmented switcher. `label` is also used for the
// "Streaming from" status line so it doesn't show the raw signal value. Icons
// come from lucide-solid (public icon set) rather than hand-rolled SVG.
const SOURCES: { id: MediaSourceType; label: string; icon: Component<{ class?: string }> }[] = [
	{ id: "camera", label: "Camera", icon: Camera },
	{ id: "screen", label: "Screen", icon: Monitor },
];

// The name a sent track is recorded under. Each subscriber gets its own
// writer with its own groups, so the subscription is part of the name.
function sentName(writer: TrackWriter): string {
	return `${writer.trackName} sent #${writer.subscribeId}`;
}

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
	const [resolution, setResolution] = createSignal<keyof typeof RESOLUTIONS>("720p");
	const [framerate, setFramerate] = createSignal<(typeof FRAMERATES)[number]>(30);
	const [bitrate, setBitrate] = createSignal(2_500_000);

	// Live stats overlay (#139): fps + media bitrate from a 1s rolling meter, plus
	// the encoder's queue depth sampled on the same tick. Cleared on stop.
	const [encQueue, setEncQueue] = createSignal(0);
	const videoStats = createStatsTicker(
		1000,
		() => setEncQueue(videoEncodeNode?.encodeQueueSize ?? 0),
	);
	onCleanup(() => videoStats.stop());

	let canvasEle: HTMLCanvasElement | undefined;
	let lastKeyframeTime = 0;
	// Identifies the current publish session. An encode node is reused across
	// sessions and detaches a destination only when its output reports that
	// destination is finished, so each session's encoder callback captures the
	// token it started under and reports itself finished once this moves past it.
	let publishSession = 0;
	let videoContext: VideoContext | undefined;
	let sourceNode: MediaStreamVideoSourceNode | null = null;
	let videoEncodeNode: VideoEncodeNode | undefined;
	let audioContext: AudioContext | undefined;
	let audioEncodeNode: AudioEncodeNode | undefined;
	// Audio track catalog entry — set in startStreaming if audio is available.
	let audioTrackDef: Track | undefined;
	// Set true by teardown() (unmount). startStreaming is async with several
	// awaits; if the user switches scenario mid-Start, teardown nulls the
	// contexts/nodes while startStreaming is suspended. The resumed coroutine
	// checks this after the awaits and bails (stopping the tracks it still
	// holds) before reaching the unguarded media-node wiring, which would
	// otherwise throw on the nulled videoContext and leak the MediaStream.
	let disposed = false;

	onMount(() => {
		if (canvasEle) {
			videoContext = new VideoContext({ canvas: canvasEle });

			// Set canvas dimensions based on the actual canvas size.
			setCanvasWidth(videoContext.destination.canvas.width);
			setCanvasHeight(videoContext.destination.canvas.height);

			audioContext = new AudioContext({ sampleRate: 48000 });
		}
	});

	let publishCtx: Context | undefined;
	let cancelPublish: CancelFunc | undefined;

	const startStreaming = async () => {
		[publishCtx, cancelPublish] = withCancel(background());

		if (!videoContext) {
			setError("Video context not initialized");
			return;
		}

		// Create the encode nodes lazily on first Start, not in onMount. An encode
		// node registers with its context on construction, and av-nodes'
		// context.close() disposes every registered node — whose dispose() flushes.
		// An unconfigured encoder throws InvalidStateError on flush, so a node that
		// was created but never configured (the common case in subscribe-only
		// scenarios, where Start is never clicked) logs a flush error on teardown.
		// Creating on demand means no encode node exists until the user actually
		// publishes, so teardown's context.close() has nothing unconfigured to
		// flush. Create-on-first-use so retries reuse the same node rather than leak.
		// Keyframe cadence is measured against this session's own clock. A new
		// source starts its timestamps wherever it likes, and one that begins
		// behind the previous session's last keyframe would never appear to have
		// reached the next GOP — so no keyframe, and nothing to open a fragment.
		lastKeyframeTime = 0;

		// This session's identity, captured by its encoder callback below.
		const session = ++publishSession;

		if (!videoEncodeNode) {
			videoEncodeNode = new VideoEncodeNode(videoContext, {
				isKey: (timestamp, _) => {
					// timestamp is in microseconds, so convert GOP_DURATION to microseconds
					if (timestamp - lastKeyframeTime >= GOP_DURATION * 1000) {
						lastKeyframeTime = timestamp;
						return true;
					}
					return false;
				},
			});
		}
		// Note: AudioEncodeNode is created later in the audio-setup block, NOT
		// here. av-nodes starts its encode loop on construction (async worklet
		// callback), so the node must be configured the instant it exists — see
		// the comment there for why a gap between create and configure bricks it.

		// Acquire media first — we need the actual track dimensions before configuring the encoder.
		// Apply the chosen resolution/framerate as getUserMedia ideal constraints.
		const target = RESOLUTIONS[resolution()];
		let stream: MediaStream;
		try {
			setError(null);
			stream = await getMediaStream(sourceType(), {
				width: target.width,
				height: target.height,
				frameRate: framerate(),
			});
		} catch (err) {
			// Pass the source type so a denied screen-share reads as screen-share,
			// not "Camera or microphone".
			setError(friendlyMessage(err, sourceType()));
			log.error("failed to start streaming", { err });
			return;
		}

		// Read actual video dimensions from the first real frame.
		// getSettings() can lie about dimensions (e.g. webcam rotation metadata).
		// ImageCapture.grabFrame() returns an ImageBitmap whose .width/.height
		// always reflect the actual pixel buffer the browser will give us through
		// MediaStreamTrackProcessor — so this is the ground truth.
		const videoTrack = stream.getVideoTracks()[0];
		let actualWidth: number;
		let actualHeight: number;
		if (videoTrack && "ImageCapture" in globalThis) {
			try {
				const imageCapture = new ImageCapture(videoTrack);
				const bitmap =
					await (imageCapture as unknown as { grabFrame(): Promise<ImageBitmap> })
						.grabFrame();
				actualWidth = bitmap.width;
				actualHeight = bitmap.height;
				bitmap.close();
			} catch (err) {
				log.warn("grabFrame failed, falling back to getSettings()", { err });
				const s = videoTrack.getSettings();
				actualWidth = s.width ?? canvasWidth();
				actualHeight = s.height ?? canvasHeight();
			}
		} else {
			const s = videoTrack?.getSettings();
			actualWidth = s?.width ?? canvasWidth();
			actualHeight = s?.height ?? canvasHeight();
		}

		// Always (re)compute the encoder config at the actual captured dimensions
		// with the current quality picks. videoEncoderConfig returns
		// WebCodecs-normalized values (bitrate scaled per codec), so don't cache
		// or compare — just apply the live values here at Start.
		const br = bitrate();
		const fps = framerate();
		let config: Awaited<ReturnType<typeof videoEncoderConfig>>;
		try {
			config = await videoEncoderConfig({
				width: actualWidth,
				height: actualHeight,
				bitrate: br,
				frameRate: fps,
				tryHardware: true,
			});
			videoEncodeNode.configure(config);
		} catch (err) {
			// Codec/encoder unsupported — release the camera we just acquired.
			stream.getTracks().forEach((t) => t.stop());
			setError(friendlyMessage(err));
			log.error("failed to configure video encoder", { err });
			return;
		}
		// Size the preview canvas to the actual stream dimensions.
		setCanvasWidth(actualWidth);
		setCanvasHeight(actualHeight);

		// Set up the audio encoder. Create the node and configure() it on the
		// SAME synchronous step: av-nodes starts its internal encode loop inside
		// the worklet's async addModule() callback (not in the constructor), so a
		// configure() called immediately after `new AudioEncodeNode` always beats
		// the loop's first read — the encoder is configured before any frame is
		// encoded. A gap between create and configure is fatal: if the context is
		// running, the loop encodes on an unconfigured codec, throws
		// InvalidStateError, and dies permanently (av-nodes can't recover the
		// loop). That is why the node is created here, not earlier. resume()
		// follows configure so the context only runs once the encoder is ready.
		if (audioContext) {
			try {
				const audioCfg = await audioEncoderConfig({
					sampleRate: audioContext.sampleRate,
					channels: audioContext.destination.channelCount,
				});
				// Create on first Start; reuse and reconfigure on retry.
				if (!audioEncodeNode) audioEncodeNode = new AudioEncodeNode(audioContext);
				audioEncodeNode.configure(audioCfg);
				audioTrackDef = {
					name: "audio",
					role: "audio",
					packaging: "loc",
					isLive: true,
					codec: audioCfg.codec,
					samplerate: audioCfg.sampleRate,
					channelConfig: String(audioCfg.numberOfChannels),
				};
				// resume() is still in the Start click's user gesture, so Chrome
				// honors it even after the config-probe await above.
				await audioContext.resume();
			} catch (err) {
				log.warn("audio setup failed, continuing without audio", { err });
			}
		}

		const initialTrack: Track = {
			name: "video",
			role: "video",
			packaging: "loc",
			isLive: true,
			codec: config.codec,
			width: config.width,
			height: config.height,
		};

		// The catalog describes the track completely — codec, picture size, and
		// for AVC its parameter sets — so a subscriber can configure a decoder,
		// or package the frames into a container, without waiting for media.
		// This board publishes encoded frames and nothing else; containers are
		// a consumer's concern.

		// Broadcast auto-serves the "catalog" track as MSF catalog JSON.
		const initialTracks: Track[] = audioTrackDef
			? [initialTrack, audioTrackDef]
			: [initialTrack];
		const broadcast = new Broadcast({ version: 1, tracks: initialTracks });

		// ---- Publish pipeline -------------------------------------------
		//
		// The encoder pipeline runs from Start, not from the first subscribe.
		// Attaching the encoder output before the source is wired guarantees
		// every chunk has a consumer — including the first keyframe, the only
		// one WebCodecs attaches decoderConfig to. Driving the encoder from
		// serveTrack instead meant that chunk was routinely produced while no
		// handler was attached, losing the config the muxer needs to write a
		// moov. serveTrack now only binds the sink.

		// Every attached subscriber's track writer. One encoder feeds all of
		// them: a single slot would mean a second subscriber silently displaces
		// the first. Media produced while the set is empty is dropped — this is
		// a live stream, so a late subscriber starts at the next fragment
		// rather than replaying a backlog.
		const writers = new Set<TrackWriter>();

		// The LOC path writes a group per GOP, so each subscriber needs its own
		// open group; keyed by writer and cleared when that writer detaches.
		const currentGroups = new Map<TrackWriter, GroupWriter>();
		let initDataPublished = false;

		// catalogReady resolves once the catalog is complete enough for a
		// subscriber (the egress) to describe the track on its first read — after
		// the first keyframe, and for AVC/HEVC after that keyframe's
		// decoderConfig has put initData (the parameter sets) into the catalog.
		// VP9/AV1 carry config in the codec string, so for them it resolves on the
		// first keyframe with no wait. The announce below awaits this so the
		// egress's first connect sees a usable catalog instead of racing the
		// initData and failing ~one GOP early.
		let resolveCatalogReady: () => void = () => {};
		const catalogReady = new Promise<void>((resolve) => {
			resolveCatalogReady = resolve;
		});
		let firstKeyframeHandled = false;

		// Update the catalog with a Base64 initData for the video track. Returns
		// the setCatalog promise so the announce gate can wait for initData to
		// land; the promise always settles fulfilled (errors are logged) so a
		// .finally on it is safe.
		const publishInitData = (initData: string): Promise<void> => {
			const updatedTrack: Track = { ...initialTrack, initData };
			const tracks: Track[] = audioTrackDef ? [updatedTrack, audioTrackDef] : [updatedTrack];
			// setCatalog validates synchronously before it awaits, so a rejected
			// catalog throws rather than returning a rejected promise — .catch()
			// alone would miss it, and av-nodes swallows anything thrown from the
			// encoder callback.
			try {
				return broadcast.setCatalog({ version: 1, tracks })
					.then(() =>
						log.info("publish: codec config in catalog", {
							bytes: initData.length,
						})
					)
					.catch((err: unknown) => log.error("setCatalog failed", { err }));
			} catch (err) {
				log.error("setCatalog threw", { err });
				return Promise.resolve();
			}
		};

		const { done: encodeDone } = videoEncodeNode.encodeTo({
			output: async (
				chunk: EncodedVideoChunk,
				decoderConfig?: VideoDecoderConfig,
			) => {
				// Returning an error is how a destination tells the encode node
				// it is finished, and the node then stops sending to it. A
				// stopped session has to say so: otherwise it stays attached to
				// a node built to be reused, and goes on consuming the chunks —
				// the decoder config among them — that the next session needs.
				if (session !== publishSession) {
					return new Error("publish: session ended");
				}

				{
					// Codecs that carry their configuration out of band — AVC
					// and HEVC — state it once here, so both a WebCodecs
					// decoder and the egress's packager can describe the track
					// before the first frame. VP9 and AV1 carry theirs in the
					// codec string and supply no description.
					if (decoderConfig?.description && !initDataPublished) {
						initDataPublished = true;
						const settled = publishInitData(
							encodeBase64(decoderConfig.description as ArrayBufferLike),
						);
						// The first keyframe carries the config, so the catalog
						// isn't complete until its initData lands — hold the
						// announce gate (catalogReady) until setCatalog settles.
						if (!firstKeyframeHandled) {
							firstKeyframeHandled = true;
							void settled.finally(resolveCatalogReady);
						}
					} else if (chunk.type === "key" && !firstKeyframeHandled) {
						// Config is in the codec string — the catalog was
						// complete from the start, so the gate clears now.
						firstKeyframeHandled = true;
						resolveCatalogReady();
					}

					// No subscriber: nothing to write to.
					if (writers.size === 0) return;

					// Each subscriber carries its own open group, and one failing
					// subscriber must not stop the others.
					const written = await Promise.allSettled(
						[...writers].map(async (writer) => {
							const name = sentName(writer);
							let group = currentGroups.get(writer);
							if (chunk.type === "key") {
								if (group) {
									void group.close();
									props.recorder?.groupEnded(name, group.sequence, "complete");
								}
								const [opened, openErr] = await writer.openGroup();
								if (openErr) throw openErr;
								group = opened;
								currentGroups.set(writer, opened);
								props.recorder?.groupArrived(name, opened.sequence);
							} else if (!group) {
								return; // drop delta frames until first keyframe
							}
							const writeErr = await group.writeFrame(new MediaFrame(chunk));
							if (writeErr) {
								props.recorder?.groupEnded(name, group.sequence, "aborted");
								throw writeErr;
							}
							props.recorder?.frameArrived(
								name,
								group.sequence,
								chunk.timestamp,
								chunk.byteLength,
							);
						}),
					);
					for (const result of written) {
						if (result.status === "rejected") {
							log.error("publish frame failed", { err: result.reason });
						}
					}
				}

				// Tally the published bytes for the live stats overlay…
				videoStats.mark(chunk.byteLength);
				// …and for the periodic diagnostic log line (fps + bitrate).
				encFps.mark();
				encBitrate.mark(chunk.byteLength);
			},
		});
		// The encoder loop owns the pipeline for the session; surface a failure
		// rather than let it vanish as an unhandled rejection. Its settle also
		// unblocks the announce gate — if encoding ends before the first keyframe
		// (camera died at Start, or the session was retired), startStreaming would
		// otherwise await catalogReady forever.
		encodeDone
			.finally(resolveCatalogReady)
			.catch((err: unknown) => log.error("video encode loop ended", { err }));

		// Register the video track handler. It does not drive the encoder — it
		// binds this subscriber as the sink and holds the subscription open
		// until the publish context ends.
		await broadcast.registerTrack(initialTrack, {
			async serveTrack(trackWriter) {
				writers.add(trackWriter);
				props.recorder?.markSent(sentName(trackWriter));
				log.info("publish: subscriber attached", {
					track: trackWriter.trackName,
					subscribers: writers.size,
				});
				try {
					await publishCtx!.done();
				} finally {
					writers.delete(trackWriter);
					const open = currentGroups.get(trackWriter);
					if (open) {
						props.recorder?.groupEnded(sentName(trackWriter), open.sequence, "stopped");
					}
					currentGroups.delete(trackWriter);
				}
			},
		});

		// Register audio track handler if audio is available.
		if (audioTrackDef && audioEncodeNode) {
			const capturedAudioEncodeNode = audioEncodeNode;
			await broadcast.registerTrack(audioTrackDef, {
				async serveTrack(trackWriter) {
					const name = sentName(trackWriter);
					props.recorder?.markSent(name);
					const { done } = capturedAudioEncodeNode.encodeTo({
						// Each Opus frame is independently decodable — one group per frame
						// lets subscribers join at any point without waiting for a keyframe.
						async output(chunk: EncodedAudioChunk, _?: AudioDecoderConfig) {
							const [group, err] = await trackWriter.openGroup();
							if (err) return err;
							props.recorder?.groupArrived(name, group.sequence);
							const writeErr = await group.writeFrame(new MediaFrame(chunk));
							void group.close();
							if (writeErr) {
								props.recorder?.groupEnded(name, group.sequence, "aborted");
								throw writeErr;
							}
							props.recorder?.frameArrived(
								name,
								group.sequence,
								chunk.timestamp,
								chunk.byteLength,
							);
							props.recorder?.groupEnded(name, group.sequence, "complete");
						},
					});
					await done;
				},
			});
		}

		// Scenario switch (or unmount) during the awaits above runs teardown(),
		// which nulls videoContext/videoEncodeNode and disposes the nodes. If
		// that happened, bail before the unguarded media-node wiring below —
		// new MediaStreamVideoSourceNode(undefined, …) would throw and the
		// camera/screen-share tracks we're still holding would leak.
		if (disposed) {
			stream.getTracks().forEach((t) => t.stop());
			return;
		}

		// Connect source nodes and start encoding. This happens before the
		// announce: the encoder output is already attached, so the pipeline
		// produces the init segment without needing a subscriber to trigger it.
		sourceNode = new MediaStreamVideoSourceNode(videoContext, { mediaStream: stream });
		sourceNode.connect(videoContext.destination);
		sourceNode.connect(videoEncodeNode);
		sourceNode.start();

		// Announce to relay — Broadcast routes "catalog" and "video" internally.
		// publish() is async; surface a rejected announce (e.g. an invalid path)
		// instead of letting it vanish as an unhandled rejection.
		//
		// Wait for the catalog to be complete first (see catalogReady). The egress
		// reads the catalog on connect; for AVC/HEVC that read needs the parameter
		// sets which arrive in the first keyframe's decoderConfig. Announcing
		// earlier races that initData — the first catalog read lacks it, the
		// egress's first connect fails, and it only self-heals ~one GOP later when
		// initData lands. VP9/AV1 carry config in the codec string, so catalogReady
		// for them resolves on the first keyframe with no real wait.
		await catalogReady;
		// teardown() or a session switch may have run while we waited.
		if (disposed || session !== publishSession) {
			stream.getTracks().forEach((t) => t.stop());
			return;
		}

		log.info("publish: announcing broadcast", { path: props.path() });
		mux.publish(
			publishCtx!.done(),
			props.path() as BroadcastPath,
			broadcast,
		).catch((err: unknown) => {
			log.error("mux.publish failed — broadcast not announced to relay", { err });
			setError(friendlyMessage(err));
		});

		// Route audio from the media stream into AudioEncodeNode. Only do this
		// when the encoder was actually configured above — feeding an
		// unconfigured encoder makes its worklet loop throw forever. Read the
		// encoder's own state (single source of truth) rather than mirroring it
		// in a parallel flag.
		if (audioEncodeNode && audioEncodeNode.encoderState === "configured" && audioContext) {
			try {
				const audioSource = audioContext.createMediaStreamSource(stream);
				audioSource.connect(audioEncodeNode);
			} catch (err) {
				log.warn("failed to connect audio source", { err });
			}
		}

		setIsStreaming(true);
		videoStats.start();
		log.info("started streaming", { source: sourceType() });
	};

	const stopStreaming = () => {
		// cancelPublish is only assigned inside startStreaming, but teardown()
		// calls this on unmount too — which can run before Start was ever clicked.
		cancelPublish?.();
		audioTrackDef = undefined;
		audioContext?.suspend().catch(() => {});
		if (sourceNode) {
			sourceNode.stop();
			sourceNode.dispose();
			sourceNode = null;
		}

		// Retires this session's encoder destinations. An encode node holds each
		// destination until its output reports one is finished, so a session that
		// simply stops leaves its callback attached and still receiving chunks —
		// including the one chunk carrying the decoder config, which the next
		// session then waits for forever. Moving the token past what the running
		// callbacks captured is how they learn to detach; see the encoder output.
		publishSession++;

		videoStats.stop();
		setIsStreaming(false);
	};

	// Full teardown on unmount. Scenario switches remount <ScenarioView>, so
	// without this every echo visit would leak an AudioContext, a VideoContext,
	// and both encode nodes (their internal worklet→encoder loops keep running).
	// After a few switches the leaked AudioContexts push the browser into a
	// degraded state where audio setup fails — which is how we end up routing
	// audio into an unconfigured encoder. Disposing here stops that at the source.
	const teardown = () => {
		// Signal in-flight startStreaming coroutines to bail at their next
		// post-await checkpoint (see the `disposed` check before mux.publish).
		disposed = true;
		stopStreaming();
		// Fire-and-forget: dispose/close are async but we're unmounting. The
		// nodes outlive a session by design and are only disposed here.
		videoEncodeNode?.dispose().catch(() => {});
		videoEncodeNode = undefined;
		audioEncodeNode?.dispose().catch(() => {});
		audioEncodeNode = undefined;
		videoContext?.close().catch(() => {});
		videoContext = undefined;
		audioContext?.close().catch(() => {});
		audioContext = undefined;
	};
	onCleanup(() => teardown());

	return (
		<div class="publish-board">
			<h2>Publish Board</h2>

			<div class="controls">
				<div class="source-selector">
					<label>Media Source</label>
					<div class="segmented" role="group" aria-label="Media source">
						{SOURCES.map((s) => {
							const Icon = s.icon;
							return (
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
									<Icon class="source-icon" /> {s.label}
								</button>
							);
						})}
					</div>
				</div>

				<div class="encoder-controls">
					<label>
						Quality
						<select
							value={resolution()}
							onChange={(e) =>
								setResolution(e.currentTarget.value as keyof typeof RESOLUTIONS)}
							disabled={isStreaming()}
						>
							{Object.entries(RESOLUTIONS).map(([id]) => (
								<option value={id}>{id}</option>
							))}
						</select>
					</label>
					<label>
						FPS
						<select
							value={framerate()}
							onChange={(e) => setFramerate(
								Number(e.currentTarget.value) as (typeof FRAMERATES)[number],
							)}
							disabled={isStreaming()}
						>
							{FRAMERATES.map((fps) => <option value={fps}>{fps}</option>)}
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
				<div class="status-message">
					Streaming from:{" "}
					{SOURCES.find((s) => s.id === sourceType())?.label ?? sourceType()}
				</div>
			</Show>

			<div class="video-preview">
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
				<Show when={isStreaming()}>
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
						<Show when={encQueue() > 0}>
							<div>
								<dt>queue</dt>
								<dd>{encQueue()}</dd>
							</div>
						</Show>
					</dl>
				</Show>
			</div>
		</div>
	);
}
