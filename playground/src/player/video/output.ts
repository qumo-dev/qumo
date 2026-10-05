import { createLogger, MediaTags } from "@okdaichi/media-log";
import { Pacer } from "../pacer.ts";

const log = createLogger(MediaTags.decoder);

// How many chunks the decoder may have waiting before feeding it more is put
// off. A live stream sits at or under this as long as the decoder keeps up.
const MAX_DECODE_QUEUE = 3;
// How long to wait for the decoder to take a chunk before trying anyway.
const DRAIN_TIMEOUT_MS = 5000;

/**
 * Decodes encoded video and draws it on a canvas, each frame when it is due.
 *
 * The decoder emits frames as fast as it decodes them; they are held until
 * their time comes and then drawn on the next animation frame. If several
 * come due between two animation frames, only the newest is drawn.
 */
export class VideoOutput {
	/**
	 * How long to hold a frame, in milliseconds, given its timestamp
	 * (microseconds). Unset, or a non-positive result, shows the frame at once.
	 */
	hold: ((timestamp: number) => number) | undefined;

	/** Called with a frame's timestamp (microseconds) when it is drawn. */
	onpresent: ((timestamp: number) => void) | undefined;

	readonly #canvas: HTMLCanvasElement;
	#decoder: VideoDecoder;
	readonly #pacer = new Pacer<VideoFrame>(
		(frame) => this.#show(frame),
		(frame) => frame.close(),
	);
	// The frame waiting for the next animation frame.
	#pending: VideoFrame | undefined;
	#animation: number | undefined;

	constructor(canvas: HTMLCanvasElement) {
		this.#canvas = canvas;
		this.#decoder = this.#newDecoder();
	}

	get decodeQueueSize(): number {
		return this.#decoder.state === "configured" ? this.#decoder.decodeQueueSize : 0;
	}

	configure(config: VideoDecoderConfig): void {
		// A decoder that hit an error closes itself and cannot be reused.
		if (this.#decoder.state === "closed") this.#decoder = this.#newDecoder();
		this.#decoder.configure(config);
	}

	/** Resolves when the decoder has room for another chunk. */
	async ready(): Promise<void> {
		while (this.#decoder.state === "configured" && this.decodeQueueSize > MAX_DECODE_QUEUE) {
			await this.#drained();
		}
	}

	/**
	 * Decodes `chunk` and shows it when due. Dropped until the decoder is
	 * configured. The first chunk after a configure must be a keyframe.
	 */
	decode(chunk: EncodedVideoChunk): void {
		if (this.#decoder.state !== "configured") return;
		this.#decoder.decode(chunk);
	}

	/** Discards the frames decoded but not yet shown. */
	flush(): void {
		this.#pacer.clear();
		this.#pending?.close();
		this.#pending = undefined;
		if (this.#animation !== undefined) cancelAnimationFrame(this.#animation);
		this.#animation = undefined;
	}

	close(): void {
		this.flush();
		if (this.#decoder.state !== "closed") this.#decoder.close();
	}

	#newDecoder(): VideoDecoder {
		return new VideoDecoder({
			// The pacer owns the frame from here: it is closed when it is
			// drawn, replaced, or discarded.
			output: (frame) => this.#pacer.push(frame, this.hold?.(frame.timestamp) ?? 0),
			error: (err) => log.error("video decoder error", { err }),
		});
	}

	// One wait for the decoder to take a chunk off its queue, or for the timeout.
	#drained(): Promise<void> {
		return new Promise((resolve) => {
			const decoder = this.#decoder;
			const finish = () => {
				clearTimeout(timer);
				decoder.removeEventListener("dequeue", finish);
				resolve();
			};
			const timer = setTimeout(finish, DRAIN_TIMEOUT_MS);
			decoder.addEventListener("dequeue", finish, { once: true });
		});
	}

	// A frame has come due: it replaces any frame still waiting to be drawn.
	#show(frame: VideoFrame): void {
		this.#pending?.close();
		this.#pending = frame;
		this.#animation ??= requestAnimationFrame(() => {
			this.#animation = undefined;
			const due = this.#pending;
			this.#pending = undefined;
			if (due !== undefined) this.#draw(due);
		});
	}

	#draw(frame: VideoFrame): void {
		try {
			const context = this.#canvas.getContext("2d");
			if (context === null) return;

			const { width, height } = this.#canvas;
			const fit = contain(frame.displayWidth, frame.displayHeight, width, height);
			context.clearRect(0, 0, width, height);
			context.drawImage(frame, fit.x, fit.y, fit.width, fit.height);
			this.onpresent?.(frame.timestamp);
		} finally {
			frame.close();
		}
	}
}

// The largest rectangle with the frame's aspect ratio that fits the canvas, centred.
function contain(
	frameWidth: number,
	frameHeight: number,
	canvasWidth: number,
	canvasHeight: number,
): { x: number; y: number; width: number; height: number } {
	const scale = Math.min(canvasWidth / frameWidth, canvasHeight / frameHeight);
	const width = frameWidth * scale;
	const height = frameHeight * scale;
	return { x: (canvasWidth - width) / 2, y: (canvasHeight - height) / 2, width, height };
}
