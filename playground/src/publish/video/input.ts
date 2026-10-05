import { createLogger, MediaTags } from "@okdaichi/media-log";
import { FrameLimiter, KeyframeCadence, Rebase } from "../timing.ts";
import { pickVideoConfig, videoCandidates } from "./encoder_config.ts";

const log = createLogger(MediaTags.encoder);

// Microseconds between keyframes. A subscriber starts at a keyframe, so this
// is the longest one waits for a picture; each keyframe also begins a group.
const KEYFRAME_INTERVAL = 1_000_000;
// How many frames the encoder may have waiting before one is dropped. A live
// stream sits at zero or one as long as the encoder keeps up.
const MAX_ENCODE_QUEUE = 4;

/** The track ended before it produced a frame. */
export class NoVideoFrameError extends Error {
	constructor() {
		super("the video source ended before producing a frame");
		this.name = "NoVideoFrameError";
	}
}

/** What to encode at; the picture's size comes from the track itself. */
export interface VideoWanted {
	/** Frames per second, at most. */
	framerate: number;
	/** Bits per second to aim for. */
	bitrate: number;
}

// Not in the DOM typings yet. Chrome and Safari have it; Firefox does not.
declare const MediaStreamTrackProcessor:
	| (new (init: { track: MediaStreamTrack }) => { readable: ReadableStream<VideoFrame> })
	| undefined;

// The track's frames, as they are captured.
function framesOf(track: MediaStreamTrack): ReadableStream<VideoFrame> {
	if (typeof MediaStreamTrackProcessor !== "undefined") {
		return new MediaStreamTrackProcessor({ track }).readable;
	}

	// Without it, the track is played in a video element and each picture
	// the element presents is taken from there.
	const video = document.createElement("video");
	video.muted = true;
	return new ReadableStream<VideoFrame>({
		async start() {
			video.srcObject = new MediaStream([track]);
			await video.play();
		},
		async pull(controller) {
			if (track.readyState === "ended") {
				controller.close();
				return;
			}
			const presented = await new Promise<number>((resolve) => {
				video.requestVideoFrameCallback((now) => resolve(now));
			});
			controller.enqueue(new VideoFrame(video, { timestamp: presented * 1000 }));
		},
		cancel() {
			video.srcObject = null;
		},
	});
}

/**
 * Captures a video track, shows it on a canvas and encodes it.
 *
 * One is opened per capture and closed at its end. The encoder is configured
 * for the size of the track's first frame, which is the only place the true
 * size is known: what the track reports can differ from the pixels it
 * delivers (a rotated camera, a window being resized).
 */
export class VideoInput {
	/**
	 * Called with each encoded frame, and with the decoder config when the
	 * encoder states one (on the first keyframe, and whenever it changes).
	 */
	onchunk:
		| ((chunk: EncodedVideoChunk, config: VideoDecoderConfig | undefined) => void)
		| undefined;

	/** Called when the track ends by itself, such as sharing being stopped. */
	onended: (() => void) | undefined;

	/** What the encoder was configured with, as the browser normalised it. */
	readonly config: VideoEncoderConfig;

	readonly #canvas: HTMLCanvasElement;
	readonly #reader: ReadableStreamDefaultReader<VideoFrame>;
	readonly #encoder: VideoEncoder;
	readonly #clock: () => number;
	readonly #rebase = new Rebase();
	readonly #limiter: FrameLimiter;
	readonly #cadence = new KeyframeCadence(KEYFRAME_INTERVAL);
	// The frame read while opening, until start() takes it.
	#first: VideoFrame | undefined;
	// The frame waiting for the next animation frame, to be shown.
	#pending: VideoFrame | undefined;
	#animation: number | undefined;
	#closed = false;

	/**
	 * Reads the track's first frame and configures an encoder for it.
	 *
	 * @param canvas - Where the captured picture is shown.
	 * @param clock - The run's clock, in microseconds; timestamps start from it.
	 * @throws {NoVideoFrameError} If the track ends without a frame.
	 * @throws {NoVideoCodecError} If the browser can encode none of the codecs.
	 */
	static async open(
		track: MediaStreamTrack,
		canvas: HTMLCanvasElement,
		wanted: VideoWanted,
		clock: () => number,
	): Promise<VideoInput> {
		const reader = framesOf(track).getReader();
		let first: VideoFrame | undefined;
		try {
			const { value } = await reader.read();
			if (value === undefined) throw new NoVideoFrameError();
			first = value;

			const candidates = videoCandidates(
				{ width: first.displayWidth, height: first.displayHeight, ...wanted },
				// Firefox cannot say whether an encoder is in hardware.
				!navigator.userAgent.includes("Firefox"),
			);
			const config = await pickVideoConfig(candidates);
			return new VideoInput(canvas, reader, first, config, wanted.framerate, clock);
		} catch (err) {
			first?.close();
			// reason: the capture is being abandoned; a failed cancel changes nothing.
			reader.cancel().catch(() => {});
			throw err;
		}
	}

	private constructor(
		canvas: HTMLCanvasElement,
		reader: ReadableStreamDefaultReader<VideoFrame>,
		first: VideoFrame,
		config: VideoEncoderConfig,
		framerate: number,
		clock: () => number,
	) {
		this.#canvas = canvas;
		this.#reader = reader;
		this.#first = first;
		this.config = config;
		this.#limiter = new FrameLimiter(framerate);
		this.#clock = clock;
		this.#encoder = new VideoEncoder({
			output: (chunk, meta) => this.onchunk?.(chunk, meta?.decoderConfig),
			error: (err) => log.error("video encoder error", { err }),
		});
		this.#encoder.configure(config);
	}

	/** Frames waiting in the encoder. */
	get encodeQueueSize(): number {
		return this.#encoder.state === "configured" ? this.#encoder.encodeQueueSize : 0;
	}

	/** Begins encoding and showing frames, from the first one the track gave. */
	start(): void {
		const first = this.#first;
		if (first === undefined || this.#closed) return;
		this.#first = undefined;
		this.#pump(first).catch((err: unknown) => {
			if (!this.#closed) log.error("video capture ended", { err });
		});
	}

	close(): void {
		if (this.#closed) return;
		this.#closed = true;
		this.#first?.close();
		this.#first = undefined;
		this.#pending?.close();
		this.#pending = undefined;
		if (this.#animation !== undefined) cancelAnimationFrame(this.#animation);
		this.#animation = undefined;
		// reason: the capture is over; a failed cancel changes nothing.
		this.#reader.cancel().catch(() => {});
		if (this.#encoder.state !== "closed") this.#encoder.close();
	}

	async #pump(first: VideoFrame): Promise<void> {
		this.#take(first);
		while (!this.#closed) {
			const { value } = await this.#reader.read();
			if (value === undefined) break;
			if (this.#closed) {
				value.close();
				return;
			}
			this.#take(value);
		}
		if (!this.#closed) this.onended?.();
	}

	// Encodes and shows one captured frame, which this takes ownership of.
	#take(frame: VideoFrame): void {
		const timestamp = this.#rebase.at(frame.timestamp, this.#clock());
		if (!this.#limiter.takes(timestamp)) {
			frame.close();
			return;
		}

		if (this.encodeQueueSize > MAX_ENCODE_QUEUE) {
			log.warn("video frame dropped: the encoder is behind", {
				queue: this.encodeQueueSize,
			});
		} else if (this.#encoder.state === "configured") {
			const timed = new VideoFrame(frame, { timestamp });
			try {
				this.#encoder.encode(timed, { keyFrame: this.#cadence.isKey(timestamp) });
			} finally {
				timed.close();
			}
		}
		this.#show(frame);
	}

	// The newest frame replaces any still waiting to be drawn.
	#show(frame: VideoFrame): void {
		this.#pending?.close();
		this.#pending = frame;
		this.#animation ??= requestAnimationFrame(() => {
			this.#animation = undefined;
			const due = this.#pending;
			this.#pending = undefined;
			if (due === undefined) return;
			try {
				this.#draw(due);
			} finally {
				due.close();
			}
		});
	}

	// Draws the frame as large as fits the canvas, centred, keeping its shape.
	// The canvas is given the picture's size once the run has started; the
	// frames before that must not be stretched to whatever it was.
	#draw(frame: VideoFrame): void {
		const context = this.#canvas.getContext("2d");
		if (context === null) return;
		const { width, height } = this.#canvas;
		const scale = Math.min(width / frame.displayWidth, height / frame.displayHeight);
		const drawn = { width: frame.displayWidth * scale, height: frame.displayHeight * scale };
		context.clearRect(0, 0, width, height);
		context.drawImage(
			frame,
			(width - drawn.width) / 2,
			(height - drawn.height) / 2,
			drawn.width,
			drawn.height,
		);
	}
}
