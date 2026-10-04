import { AudioDecodeNode, VideoContext, VideoDecodeNode } from "@okdaichi/av-nodes";
import { background, withCancel } from "@okdaichi/golikejs/context";
import { createLogger, MediaTags } from "@okdaichi/media-log";
import { decoderConfigs } from "./catalog.ts";
import {
	type GroupObserver,
	type GroupOutcome,
	readFrames,
	TrackEndedError,
	type TrackFrame,
	type Unpack,
} from "./reader.ts";
import type { Track, TrackSource } from "./source.ts";
import { delayFor, Sync } from "./sync.ts";
import { VideoPaceNode } from "./video_pace_node.ts";

const log = createLogger("subscribe");
const videoLog = createLogger(MediaTags.decoder);
const audioLog = createLogger(MediaTags.audio);

/** Told what happens to each track's groups and frames during playback. */
export interface PlaybackObserver {
	groupArrived(track: string, group: number): void;
	frameArrived(track: string, group: number, timestamp: number, bytes: number): void;
	groupEnded(track: string, group: number, outcome: GroupOutcome): void;
	/** A frame reached its output: the canvas for video, the decoder for audio. */
	frameRendered(track: string, timestamp: number): void;
}

export interface ViewerInit {
	canvas: HTMLCanvasElement;
	unpack: Unpack;
	observer?: PlaybackObserver;
	/** The video decoder was (re)configured from a catalog. */
	onVideoConfig?(config: VideoDecoderConfig): void;
	/** A video chunk of `byteLength` bytes was handed to the decoder. */
	onVideoChunk?(byteLength: number): void;
	/** Playback stopped because the broadcast could not be played. */
	onFail?(err: Error): void;
}

/**
 * Plays one broadcast from a {@link TrackSource} onto a canvas and the default
 * audio output. It holds no UI state; the owner drives it with start/stop and
 * listens through the {@link ViewerInit} callbacks.
 */
export class Viewer {
	readonly #init: ViewerInit;
	readonly #video: VideoDecodeNode;
	readonly #pace: VideoPaceNode;
	#volume = 1;
	// Audio output of the current run; undefined while stopped.
	#audio: AudioOutput | undefined;
	// Cancels the current run; undefined while stopped.
	#cancel: (() => void) | undefined;

	constructor(init: ViewerInit) {
		this.#init = init;

		// Decoded frames are held by the pace node until they are due.
		const videoContext = new VideoContext({ canvas: init.canvas });
		this.#video = new VideoDecodeNode(videoContext);
		this.#pace = new VideoPaceNode();
		this.#video.connect(this.#pace);
		this.#pace.connect(videoContext.destination);

		const { observer } = init;
		if (observer) {
			this.#pace.onpresent = (timestamp) => observer.frameRendered("video", timestamp);
		}
	}

	/** Playback level, 0 to 1. Client-side only; no effect on the stream. */
	set volume(value: number) {
		this.#volume = value;
		if (this.#audio) this.#audio.node.gain.value = value;
	}

	get decodeQueueSize(): number {
		return this.#video.decodeQueueSize;
	}

	/**
	 * Starts playing `broadcast`. Must be called from a user gesture's async
	 * chain, since it opens the audio output.
	 */
	start(source: TrackSource, broadcast: string): void {
		this.stop();
		const [ctx, cancel] = withCancel(background());
		this.#cancel = cancel;

		void this.#play({
			source,
			broadcast,
			done: ctx.done(),
			playing: () => this.#cancel === cancel,
			// Opens when the first catalog has configured the video decoder;
			// no frame is fed to a decoder before that.
			configured: gate(),
			observer: this.#init.observer,
		});
	}

	stop(): void {
		this.#cancel?.();
		this.#cancel = undefined;
		this.#pace.hold = undefined;
		this.#pace.flush();

		const audio = this.#audio;
		this.#audio = undefined;
		if (audio) {
			audio.node.dispose();
			// reason: close only rejects on an already-closed context.
			audio.context.close().catch(() => {});
		}
	}

	async #play(run: Run): Promise<void> {
		// Audio and video trail the live edge by the same delay, sized once
		// per run: the audio cushion cannot be resized while it plays.
		const delay = delayFor(await run.source.rtt());
		if (!run.playing()) return;
		log.info("playback delay", { ms: Math.round(delay) });

		// Each track keeps its own clock. Their timestamps need not share an
		// origin: a browser publisher stamps video from its video context and
		// audio from its audio worklet, which start at different moments. On
		// one clock the track with the older origin would look permanently
		// late. What the tracks share is the delay.
		const clocks = { video: new Sync(delay), audio: new Sync(delay) };
		this.#pace.hold = (timestamp) => {
			const due = clocks.video.due(timestamp);
			return due === undefined ? 0 : due - performance.now();
		};
		const audio = this.#openAudio(delay);
		const playback: Playback = { ...run, clocks, audio };

		void this.#playCatalog(playback);
		void this.#playVideo(playback);
		void this.#playAudio(playback);
	}

	#openAudio(delay: number): AudioOutput {
		// Match the source rate (AAC 48 kHz). av-nodes' worklet converts frame
		// PTS -> sample offset using context.sampleRate; if the context runs at
		// the system default (e.g. 44100) every 1024-sample block is scheduled
		// at the wrong offset under timestamp scheduling. The publisher pins
		// 48000 too.
		const context = new AudioContext({ sampleRate: 48000 });
		// The node's latency is its jitter cushion, fixed at construction, and
		// a context can register the worklet only once: hence one context and
		// one node per run.
		const node = new AudioDecodeNode(context, { latency: delay });
		node.connect(context.destination);
		node.gain.value = this.#volume;

		this.#audio = { context, node };
		return this.#audio;
	}

	#fail(run: Run, err: Error): void {
		if (!run.playing()) return;
		this.stop();
		this.#init.onFail?.(err);
	}

	async #playCatalog(run: Playback): Promise<void> {
		const [track, err] = await run.source.subscribe(run.broadcast, "catalog");
		if (track === undefined) {
			if (!run.playing()) return;
			// TrackNotFound here is the common "nobody is publishing to this
			// path yet" case. With no catalog there is no stream to recover,
			// so stop and let the owner offer a retry.
			log.warn("catalog subscribe failed", { err });
			this.#fail(run, err);
			return;
		}

		// The catalog is not media: its groups carry no timestamps to pace
		// by, so it is read plainly, one group after another.
		try {
			while (run.playing()) {
				const [group, err] = await track.acceptGroup(run.done);
				if (group === undefined) {
					if (run.playing()) log.warn("catalog acceptGroup failed", { err });
					break;
				}
				for await (const frame of group.frames()) {
					this.#configure(run, frame.bytes);
				}
			}
		} catch (err) {
			if (run.playing()) log.warn("catalog track error", { err });
		} finally {
			track.close();
		}
	}

	#configure(run: Playback, catalog: Uint8Array): void {
		const configs = decoderConfigs(catalog);
		if (configs.video) {
			this.#video.configure(configs.video);
			log.info("video decoder configured", { codec: configs.video.codec });
			this.#init.onVideoConfig?.(configs.video);
			log.info("catalog received", { videoCodec: configs.video.codec });
			run.configured.open();
		}
		if (configs.audio) {
			run.audio.node.configure(configs.audio);
			audioLog.info("audio decoder configured", { codec: configs.audio.codec });
		}
	}

	async #playVideo(run: Playback): Promise<void> {
		const [track, err] = await run.source.subscribe(run.broadcast, "video");
		if (track === undefined) {
			if (!run.playing()) return;
			videoLog.warn("subscribe failed", { err });
			this.#fail(run, err);
			return;
		}

		const { unpack, onVideoChunk } = this.#init;
		this.#video.decodeFrom(chunkStream(run, track, unpack, videoLog, "video", (frame) => {
			onVideoChunk?.(frame.data.byteLength);
			return new EncodedVideoChunk({
				// Each group opens on a keyframe.
				type: frame.index === 0 ? "key" : "delta",
				timestamp: frame.timestamp,
				data: frame.data,
			});
		}, () => videoLog.info("catalog ready, starting decode loop")));
	}

	// Audio is optional: a broadcast without an audio track plays video only.
	async #playAudio(run: Playback): Promise<void> {
		const [track, err] = await run.source.subscribe(run.broadcast, "audio");
		if (track === undefined) {
			if (!run.playing()) return;
			audioLog.warn("subscribe failed", { err });
			return;
		}

		try {
			await run.audio.context.resume();
		} catch {
			// reason: resume rejects once stop() has closed the context; the
			// check below then ends the subscription.
		}
		if (!run.playing()) {
			track.close();
			return;
		}

		const { unpack, observer } = this.#init;
		run.audio.node.decodeFrom(chunkStream(run, track, unpack, audioLog, "audio", (frame) => {
			// Decoded audio goes straight into the output graph, so handing
			// a frame to the decoder is the last point it can be observed.
			observer?.frameRendered("audio", frame.timestamp);
			// Audio frames are all independently decodable.
			return new EncodedAudioChunk({
				type: "key",
				timestamp: frame.timestamp,
				data: frame.data,
			});
		}));
	}
}

interface AudioOutput {
	readonly context: AudioContext;
	readonly node: AudioDecodeNode;
}

// One start()-to-stop() span of playback.
interface Run {
	readonly source: TrackSource;
	readonly broadcast: string;
	/** Resolves when the run is stopped. */
	readonly done: Promise<void>;
	/** False once the run has been stopped or superseded. */
	readonly playing: () => boolean;
	readonly configured: Gate;
	readonly observer: PlaybackObserver | undefined;
}

function groupObserver(observer: PlaybackObserver, track: string): GroupObserver {
	return {
		groupArrived: (group) => observer.groupArrived(track, group),
		frameArrived: (group, timestamp, bytes) =>
			observer.frameArrived(track, group, timestamp, bytes),
		groupEnded: (group, outcome) => observer.groupEnded(track, group, outcome),
	};
}

// A run once its timing and audio output exist.
interface Playback extends Run {
	readonly clocks: Readonly<Record<"video" | "audio", Sync>>;
	readonly audio: AudioOutput;
}

interface Gate {
	readonly promise: Promise<void>;
	open(): void;
}

function gate(): Gate {
	// The executor runs synchronously, so `open` is the resolver by the time
	// this returns.
	let open: () => void = () => {};
	const promise = new Promise<void>((resolve) => {
		open = resolve;
	});
	return { promise, open };
}

// Turns a track into the stream of encoded chunks a decode node reads from.
// The subscription is ended when the stream finishes, however it finishes.
function chunkStream<T>(
	run: Playback,
	track: Track,
	unpack: Unpack,
	logger: ReturnType<typeof createLogger>,
	label: "video" | "audio",
	toChunk: (frame: TrackFrame) => T,
	onReady?: () => void,
): ReadableStream<T> {
	// Arrivals drive the track's playback clock, so they are observed here,
	// before the reader decides what to skip, whether or not anyone else is
	// watching.
	const sync = run.clocks[label];
	const watcher = run.observer && groupObserver(run.observer, label);
	const observer: GroupObserver = {
		groupArrived: (group) => watcher?.groupArrived(group),
		frameArrived(group, timestamp, bytes) {
			sync.observe(timestamp, performance.now());
			watcher?.frameArrived(group, timestamp, bytes);
		},
		groupEnded: (group, outcome) => watcher?.groupEnded(group, outcome),
	};
	// How far past its due time a frame would be if it were delivered now.
	const lateness = (timestamp: number) => {
		const due = sync.due(timestamp);
		return due === undefined ? 0 : performance.now() - due;
	};
	return new ReadableStream<T>({
		async start(controller) {
			try {
				// A stop before the first catalog must still reach the finally
				// below, so the wait gives way to it.
				await Promise.race([run.configured.promise, run.done]);
				if (run.playing()) onReady?.();

				// A group is waited for no longer than playback trails the
				// live edge: past that its frames could not be shown anyway.
				const options = { unpack, maxAge: sync.delay, observer, lateness };
				for await (const frame of readFrames(track, run.done, options)) {
					controller.enqueue(toChunk(frame));
				}
			} catch (err) {
				if (err instanceof TrackEndedError) {
					if (run.playing()) logger.error("track ended", { err: err.cause });
					controller.close();
				} else if (run.playing()) {
					logger.error(`${label} track error`, { err });
					controller.error(err);
				} else {
					controller.close();
				}
			} finally {
				track.close();
			}
		},
	});
}
