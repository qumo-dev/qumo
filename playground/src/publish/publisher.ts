import type { BroadcastPath, TrackMux, TrackWriter } from "@qumo/moq";
import { Broadcast, type Track } from "@qumo/moq/msf";
import { background, withCancel } from "@okdaichi/golikejs/context";
import { createMediaLogger, MediaTags } from "@okdaichi/media-log";
import { pickAudioConfig } from "./audio/encoder_config.ts";
import { AudioInput } from "./audio/input.ts";
import { Fanout, type SendObserver } from "./fanout.ts";
import { MediaFrame } from "./media_frame.ts";
import { VideoInput, type VideoWanted } from "./video/input.ts";

// The meters flush one diagnostic fps and bitrate line a second.
const log = createMediaLogger(MediaTags.encoder);
const encodedFps = log.meter.fps("encode");
const encodedBitrate = log.meter.bitrate("egress");

/** The stream has no video track to publish. */
export class NoVideoTrackError extends Error {
	constructor() {
		super("the media stream has no video track");
		this.name = "NoVideoTrackError";
	}
}

export interface PublisherInit {
	/** Where the broadcast is announced. */
	mux: TrackMux;
	/** Where the captured picture is shown. */
	canvas: HTMLCanvasElement;
	/** Told of every group and frame sent. */
	observer?: SendObserver;
}

/** What one run publishes, and how. */
export interface PublishSettings extends VideoWanted {
	/** The broadcast's path on the relay. */
	path: string;
}

/** The size of the picture being published, in pixels. */
export interface Picture {
	width: number;
	height: number;
}

// One run: from start to stop.
interface Run {
	readonly stream: MediaStream;
	// Ends the broadcast and every subscription to it.
	readonly cancel: () => void;
	// Lets go of a start still waiting for the first keyframe.
	release: () => void;
	video: VideoInput | undefined;
	stopped: boolean;
}

// The name a sent track is recorded under. Each subscriber gets its own
// writer with its own groups, so the subscription is part of the name.
function sentName(writer: TrackWriter): string {
	return `${writer.trackName} sent #${writer.subscribeId}`;
}

function encodeBase64(bytes: AllowSharedBufferSource): string {
	const view = ArrayBuffer.isView(bytes)
		? new Uint8Array(bytes.buffer, bytes.byteOffset, bytes.byteLength)
		: new Uint8Array(bytes);
	let binary = "";
	for (const byte of view) binary += String.fromCharCode(byte);
	return btoa(binary);
}

/**
 * Publishes a media stream as a broadcast: captures it, encodes it, describes
 * it in a catalog and sends each track to whoever subscribes.
 *
 * It has no UI of its own beyond drawing the captured picture on the canvas
 * it is given. One publisher serves any number of runs, one at a time.
 */
export class Publisher {
	/** Called with the size of each encoded video frame. */
	onencoded: ((bytes: number) => void) | undefined;

	/** Called when a run that had started fails; the run is stopped. */
	onerror: ((err: unknown) => void) | undefined;

	/** Called when a run ends by itself, such as sharing being stopped. */
	onended: (() => void) | undefined;

	readonly #init: PublisherInit;
	// Made on the first run with audio, then reused: opening an audio
	// context and loading its worklet is not something to redo per run.
	#audio: AudioInput | undefined;
	#run: Run | undefined;

	constructor(init: PublisherInit) {
		this.#init = init;
	}

	/** Video frames waiting in the encoder. */
	get encodeQueueSize(): number {
		return this.#run?.video?.encodeQueueSize ?? 0;
	}

	/**
	 * Starts publishing `stream`, ending any run in progress. The stream is
	 * the publisher's from here: its tracks are stopped when the run ends,
	 * however it ends.
	 *
	 * Resolves once the broadcast is announced, which is after the first
	 * keyframe: only then is the catalog complete enough for a subscriber to
	 * use. Audio that cannot be captured or encoded is left out, and the
	 * broadcast is video only.
	 *
	 * @returns The picture's size, or undefined if the run was stopped
	 *   before it began.
	 * @throws {NoVideoTrackError} If the stream has no video.
	 * @throws {NoVideoFrameError} If the video ends without a frame.
	 * @throws {NoVideoCodecError} If the browser can encode none of the codecs.
	 */
	async start(stream: MediaStream, settings: PublishSettings): Promise<Picture | undefined> {
		this.stop();

		const [context, cancel] = withCancel(background());
		const run: Run = { stream, cancel, release: () => {}, video: undefined, stopped: false };
		this.#run = run;

		// Both tracks are stamped from one clock, which starts with the run.
		const origin = performance.now();
		const clock = () => (performance.now() - origin) * 1000;

		try {
			const track = stream.getVideoTracks()[0];
			if (track === undefined) throw new NoVideoTrackError();

			const video = await VideoInput.open(track, this.#init.canvas, settings, clock);
			if (run.stopped) {
				video.close();
				return undefined;
			}
			run.video = video;
			log.info("publish: video encoder", { config: video.config });
			video.onended = () => {
				this.#end(run);
				this.onended?.();
			};

			const audioTrack = await this.#startAudio(run, clock);
			if (run.stopped) return undefined;

			const videoTrack: Track = {
				name: "video",
				role: "video",
				packaging: "loc",
				isLive: true,
				codec: video.config.codec,
				width: video.config.width,
				height: video.config.height,
			};
			const tracks = (described: Track): Track[] =>
				audioTrack === undefined ? [described] : [described, audioTrack];

			// The catalog describes each track completely (codec, picture
			// size, and for AVC its parameter sets), so a subscriber can
			// configure a decoder, or package the frames into a container,
			// without waiting for media. The broadcast serves it as the
			// "catalog" track.
			const broadcast = new Broadcast({ version: 1, tracks: tracks(videoTrack) });

			const onerror = (name: string, err: Error) => {
				log.error("publish frame failed", { track: name, err });
			};
			const observer = this.#init.observer;
			const videoFanout = new Fanout<MediaFrame>({ grouping: "keyframe", observer, onerror });
			// Each Opus frame can be decoded by itself, so each is a group.
			const audioFanout = new Fanout<MediaFrame>({ grouping: "frame", observer, onerror });

			// Opens once the catalog is complete enough for a subscriber (the
			// HLS egress reads it on connect) to describe the video track:
			// on the first keyframe, and for codecs whose configuration is
			// stated apart from the stream, once that is in the catalog.
			// Announcing sooner makes the first catalog read lack it.
			const catalogReady = new Promise<void>((resolve) => {
				run.release = resolve;
			});
			let described = false;
			let seenKeyframe = false;

			video.onchunk = (chunk, config) => {
				if (config?.description !== undefined && !described) {
					described = true;
					const initData = encodeBase64(config.description);
					const settled = this.#describe(broadcast, tracks({ ...videoTrack, initData }));
					if (!seenKeyframe) {
						seenKeyframe = true;
						void settled.then(run.release);
					}
				} else if (chunk.type === "key" && !seenKeyframe) {
					// The configuration is in the codec string or in the
					// stream: the catalog was complete from the start.
					seenKeyframe = true;
					run.release();
				}

				this.onencoded?.(chunk.byteLength);
				encodedFps.mark();
				encodedBitrate.mark(chunk.byteLength);

				void videoFanout.send(new MediaFrame(chunk), {
					key: chunk.type === "key",
					timestamp: chunk.timestamp,
					bytes: chunk.byteLength,
				});
			};
			if (audioTrack !== undefined && this.#audio !== undefined) {
				this.#audio.onchunk = (chunk) => {
					void audioFanout.send(new MediaFrame(chunk), {
						key: true,
						timestamp: chunk.timestamp,
						bytes: chunk.byteLength,
					});
				};
			}

			// A subscription lasts until the run ends or the subscriber leaves.
			const serve = (fanout: Fanout<MediaFrame>) => ({
				async serveTrack(writer: TrackWriter) {
					fanout.add(sentName(writer), writer);
					log.info("publish: subscriber attached", {
						track: writer.trackName,
						subscribers: fanout.size,
					});
					try {
						await Promise.race([context.done(), writer.context.done()]);
					} finally {
						fanout.remove(writer);
					}
				},
			});
			await broadcast.registerTrack(videoTrack, serve(videoFanout));
			if (audioTrack !== undefined) {
				await broadcast.registerTrack(audioTrack, serve(audioFanout));
			}
			if (run.stopped) return undefined;

			// Encoding runs from here, not from the first subscription: the
			// first keyframe is what completes the catalog.
			video.start();
			await catalogReady;
			if (run.stopped) return undefined;

			log.info("publish: announcing broadcast", { path: settings.path });
			// reason: the path is taken as typed by the user; the relay is
			// the one that accepts or rejects it.
			this.#init.mux.publish(context.done(), settings.path as BroadcastPath, broadcast)
				.catch((err: unknown) => {
					if (run.stopped) return;
					log.error("publish: the broadcast was not announced", { err });
					this.#end(run);
					this.onerror?.(err);
				});

			return { width: video.config.width, height: video.config.height };
		} catch (err) {
			this.#end(run);
			throw err;
		}
	}

	/** Ends the run in progress, if there is one. */
	stop(): void {
		if (this.#run !== undefined) this.#end(this.#run);
	}

	/** Stops, and lets go of what is kept between runs. */
	close(): void {
		this.stop();
		this.#audio?.close();
		this.#audio = undefined;
	}

	// Starts capturing the run's audio, and returns its catalog entry.
	// Undefined when the stream has no audio or it cannot be captured.
	async #startAudio(run: Run, clock: () => number): Promise<Track | undefined> {
		if (run.stream.getAudioTracks().length === 0) return undefined;
		try {
			this.#audio ??= new AudioInput();
			const audio = this.#audio;
			const config = await pickAudioConfig(audio.sampleRate, audio.channels);
			if (run.stopped) return undefined;
			await audio.start(run.stream, config, clock);
			log.info("publish: audio encoder", { config });
			return {
				name: "audio",
				role: "audio",
				packaging: "loc",
				isLive: true,
				codec: config.codec,
				samplerate: config.sampleRate,
				channelConfig: String(config.numberOfChannels),
			};
		} catch (err) {
			log.warn("audio setup failed, continuing without audio", { err });
			return undefined;
		}
	}

	// Replaces the catalog. Resolves either way: a catalog that could not be
	// updated is logged, and the broadcast goes on with the one it has.
	async #describe(broadcast: Broadcast, tracks: Track[]): Promise<void> {
		try {
			await broadcast.setCatalog({ version: 1, tracks });
			log.info("publish: codec config in catalog");
		} catch (err) {
			log.error("publish: the catalog could not be updated", { err });
		}
	}

	#end(run: Run): void {
		if (run.stopped) return;
		run.stopped = true;
		if (this.#run === run) this.#run = undefined;
		run.cancel();
		run.release();
		run.video?.close();
		if (this.#audio !== undefined) {
			this.#audio.stop();
			this.#audio.onchunk = undefined;
		}
		for (const track of run.stream.getTracks()) track.stop();
	}
}
