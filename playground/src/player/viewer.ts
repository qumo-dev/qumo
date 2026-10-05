import { background, withCancel } from "@okdaichi/golikejs/context";
import { createLogger, MediaTags } from "@okdaichi/media-log";
import { type AudioBufferStats, AudioOutput } from "./audio/output.ts";
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
import { delayFor, delayForJitter, raisedDelay, steppedDelay, Sync, waitBudget } from "./sync.ts";
import { VideoOutput } from "./video/output.ts";

const log = createLogger("subscribe");
const videoLog = createLogger(MediaTags.decoder);
const audioLog = createLogger(MediaTags.audio);

/** Told what happens to each track's groups and frames during playback. */
export interface PlaybackObserver {
	groupArrived(track: string, group: number): void;
	frameArrived(track: string, group: number, timestamp: number, bytes: number): void;
	groupEnded(track: string, group: number, outcome: GroupOutcome): void;
	/** A frame reached its output: drawn on the canvas, or handed to the audio decoder. */
	frameRendered(track: string, timestamp: number): void;
	/**
	 * A frame was handed on after waiting `held` milliseconds in the reader
	 * for an earlier group.
	 */
	frameHeld?(track: string, held: number): void;
	/** The audio jitter buffer's state, several times a second while playing. */
	audioBuffer?(stats: AudioBufferStats): void;
	/** The playback delay and the arrival jitter it is sized from, in milliseconds. */
	playbackTiming?(timing: PlaybackTiming): void;
	/**
	 * A new stretch of playback is starting: what comes next does not carry on
	 * from what was reported before.
	 */
	playbackStarted?(): void;
	/** The stretch of playback last reported as starting has ended. */
	playbackStopped?(): void;
}

export interface PlaybackTiming {
	/** How far playback trails the live edge. */
	delay: number;
	/** How late audio has recently arrived against its fastest arrival. */
	audioJitter: number;
	/** The same for video. */
	videoJitter: number;
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
	readonly #video: VideoOutput;
	// Made with the viewer, not with each run: opening an audio output and
	// loading its worklet takes a few hundred milliseconds, during which audio
	// that has already arrived would have nowhere to go.
	readonly #audio: AudioOutput;
	// Cancels the current run; undefined while stopped.
	#cancel: (() => void) | undefined;
	// Whether the observer has been told of a start it has not been told the end of.
	#reportedStart = false;

	constructor(init: ViewerInit) {
		this.#init = init;
		this.#video = new VideoOutput(init.canvas);
		this.#audio = new AudioOutput(delayFor(undefined));

		const { observer } = init;
		if (observer) {
			this.#video.onpresent = (timestamp) => observer.frameRendered("video", timestamp);
		}
	}

	/** Playback level, 0 to 1. Client-side only; no effect on the stream. */
	set volume(value: number) {
		this.#audio.volume = value;
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

	/** Stops playback and releases the decoders and the audio output for good. */
	close(): void {
		this.stop();
		this.#video.close();
		this.#audio.close();
	}

	stop(): void {
		this.#cancel?.();
		this.#cancel = undefined;
		if (this.#reportedStart) {
			this.#reportedStart = false;
			this.#init.observer?.playbackStopped?.();
		}
		this.#video.hold = undefined;
		this.#video.flush();

		this.#audio.onstats = undefined;
		this.#audio.suspend();
	}

	async #play(run: Run): Promise<void> {
		// Audio and video trail the live edge by the same delay. It starts
		// from the connection's round-trip time and follows how unevenly the
		// audio actually arrives: a source that sends several frames at once,
		// or a reader that was briefly busy, needs more than the network
		// alone says. It is not lowered again during the run: shortening it
		// means skipping audio, and a delay that hunts up and down is worse
		// to listen to than one that settles a little long.
		const floor = delayFor(await run.source.rtt());
		let delay = floor;
		if (!run.playing()) return;
		log.info("playback delay", { ms: Math.round(delay) });

		// Each track keeps its own clock. Their timestamps need not share an
		// origin: a browser publisher stamps video from its video context and
		// audio from its audio worklet, which start at different moments. On
		// one clock the track with the older origin would look permanently
		// late. What the tracks share is the delay.
		const clocks = { video: new Sync(delay), audio: new Sync(delay) };
		this.#video.hold = (timestamp) => {
			const due = clocks.video.due(timestamp);
			return due === undefined ? 0 : due - performance.now();
		};
		const audio = this.#audio;
		audio.reset(delay);
		this.#reportedStart = true;
		run.observer?.playbackStarted?.();
		const raise = (to: number, why: string) => {
			if (to <= delay) return;
			delay = to;
			audio.setLatency(delay);
			clocks.video.delay = delay;
			clocks.audio.delay = delay;
			log.info(`playback delay raised: ${why}`, { ms: Math.round(delay) });
		};

		// The output counts underruns since it was made, across runs.
		let underruns: number | undefined;
		audio.onstats = (stats) => {
			run.observer?.audioBuffer?.(stats);
			underruns ??= stats.underruns;

			// Ahead of trouble: the arrivals show how long a wait the buffer
			// has to bridge, before it has had to bridge one.
			const jitter = clocks.audio.jitter;
			raise(steppedDelay(delay, delayForJitter(jitter, floor)), "audio arrives unevenly");

			// And after it: running dry anyway means something held the audio
			// up after it had arrived, which the arrivals cannot show.
			if (stats.underruns > underruns) {
				underruns = stats.underruns;
				raise(raisedDelay(delay), "the audio buffer ran dry");
			}

			run.observer?.playbackTiming?.({
				delay,
				audioJitter: jitter,
				videoJitter: clocks.video.jitter,
			});
		};
		const playback: Playback = {
			...run,
			clocks,
			audio,
			configs: {},
			keyframe: { needed: true },
		};

		void this.#playCatalog(playback);
		void this.#playVideo(playback);
		void this.#playAudio(playback);
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
		// A catalog is sent again whenever anything in it changes. Configuring
		// a decoder makes it start over, a video decoder from a keyframe it
		// will not see until the next group, so one whose configuration is
		// unchanged is left alone.
		const configs = decoderConfigs(catalog);
		const videoKey = configs.video && configKey(configs.video);
		if (configs.video && videoKey !== run.configs.video) {
			run.configs.video = videoKey;
			this.#video.configure(configs.video);
			run.keyframe.needed = true;
			log.info("video decoder configured", { codec: configs.video.codec });
			this.#init.onVideoConfig?.(configs.video);
			log.info("catalog received", { videoCodec: configs.video.codec });
			run.configured.open();
		}
		const audioKey = configs.audio && configKey(configs.audio);
		if (configs.audio && audioKey !== run.configs.audio) {
			run.configs.audio = audioKey;
			run.audio.configure(configs.audio);
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

		const video = this.#video;
		const { onVideoChunk } = this.#init;
		await this.#pump(run, track, "video", videoLog, async (frame) => {
			// Feed the decoder no faster than it decodes, so a burst is held
			// back here rather than piling up as decoded frames.
			await video.ready();
			// A decoder configured in the middle of a group cannot take the
			// rest of it: it starts again at the next group's keyframe.
			if (run.keyframe.needed) {
				if (frame.index !== 0) return;
				run.keyframe.needed = false;
			}
			onVideoChunk?.(frame.data.byteLength);
			video.decode(
				new EncodedVideoChunk({
					// Each group opens on a keyframe.
					type: frame.index === 0 ? "key" : "delta",
					timestamp: frame.timestamp,
					data: frame.data,
				}),
			);
		});
	}

	// Audio is optional: a broadcast without an audio track plays video only.
	async #playAudio(run: Playback): Promise<void> {
		// The output is started before the track is asked for. Starting it
		// can take seconds the first time, and audio that arrived meanwhile
		// would all be handed over at once, only to be passed over.
		try {
			await run.audio.resume();
		} catch {
			// reason: resume rejects once close() has closed the context;
			// playing() below is then false as well.
		}
		if (!run.playing()) return;

		const [track, err] = await run.source.subscribe(run.broadcast, "audio");
		if (track === undefined) {
			if (!run.playing()) return;
			audioLog.warn("subscribe failed", { err });
			return;
		}
		if (!run.playing()) {
			track.close();
			return;
		}

		const { observer } = this.#init;
		await this.#pump(run, track, "audio", audioLog, (frame) => {
			// Decoded audio goes straight to the output thread, so handing a
			// frame to the decoder is the last point it can be observed here.
			observer?.frameRendered("audio", frame.timestamp);
			run.audio.decode(
				new EncodedAudioChunk({
					// Audio frames are all independently decodable.
					type: "key",
					timestamp: frame.timestamp,
					data: frame.data,
				}),
			);
		});
	}

	// Reads a track's frames and hands each to `deliver`, until playback
	// stops or the track ends. The subscription is ended either way.
	async #pump(
		run: Playback,
		track: Track,
		label: "video" | "audio",
		logger: ReturnType<typeof createLogger>,
		deliver: (frame: TrackFrame) => void | Promise<void>,
	): Promise<void> {
		// Arrivals drive the track's playback clock, so they are observed
		// here, before the reader decides what to skip, whether or not anyone
		// else is watching.
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

		try {
			// No frame goes to a decoder before the first catalog has
			// configured it. A stop before then must still reach the finally
			// below, so the wait gives way to it.
			await Promise.race([run.configured.promise, run.done]);
			if (run.playing()) logger.info("catalog ready, starting decode loop");

			const options = {
				unpack: this.#init.unpack,
				// Asked each time: the delay can be raised during the run.
				maxAge: () => waitBudget(label, sync.delay),
				observer,
				lateness,
			};
			for await (const frame of readFrames(track, run.done, options)) {
				if (frame.held >= MIN_HOLD_MS) run.observer?.frameHeld?.(label, frame.held);
				await deliver(frame);
			}
		} catch (err) {
			if (!run.playing()) return;
			if (err instanceof TrackEndedError) {
				logger.error("track ended", { err: err.cause });
			} else {
				logger.error(`${label} track error`, { err });
			}
		} finally {
			track.close();
		}
	}
}

// A frame held in the reader for less than this was not kept waiting in any
// sense worth reporting.
const MIN_HOLD_MS = 20;

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
	/** The {@link configKey} of each decoder's configuration in use. */
	readonly configs: { video?: string; audio?: string };
	/** Whether the video decoder has to be given a keyframe next. */
	readonly keyframe: { needed: boolean };
}

// A string that is the same for two decoder configurations exactly when they
// are the same, codec description bytes included.
function configKey(config: VideoDecoderConfig | AudioDecoderConfig): string {
	return JSON.stringify(
		config,
		(_, value: unknown) => ArrayBuffer.isView(value) ? Array.from(bytesOf(value)) : value,
	);
}

function bytesOf(view: ArrayBufferView): Uint8Array {
	return new Uint8Array(view.buffer, view.byteOffset, view.byteLength);
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
