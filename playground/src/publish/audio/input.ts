import { createLogger, MediaTags } from "@okdaichi/media-log";
import { Rebase } from "../timing.ts";
import {
	BLOCK_MS,
	type CaptureBlock,
	type CaptureMessage,
	type CaptureOptions,
	PROCESSOR_NAME,
} from "./worklet.ts";
// reason: Vite resolves `?worker&url` to the URL of the bundled worklet. The
// type checker reads the file itself, which has no default export.
// @ts-expect-error TS1192
import workletUrl from "./worklet.ts?worker&url";

const log = createLogger(MediaTags.audio);

// The rate audio is captured and encoded at. The viewer and the ingests all
// work at 48 kHz, and the browser resamples a microphone that does not.
const SAMPLE_RATE = 48000;
// Channels encoded. A microphone with one is heard on both.
const CHANNELS = 2;
// How many blocks the encoder may have waiting before one is dropped. A live
// stream sits at zero or one as long as the encoder keeps up.
const MAX_ENCODE_QUEUE = 5;

// One capture: from start to stop.
interface Capture {
	readonly encoder: AudioEncoder;
	readonly source: MediaStreamAudioSourceNode;
	readonly rebase: Rebase;
	// The run's clock, in microseconds.
	readonly clock: () => number;
	// Samples captured so far, which is what the timestamps count.
	samples: number;
}

/**
 * Captures a stream's audio and encodes it.
 *
 * It owns its AudioContext and worklet and is made once and reused: each
 * {@link start} is a new capture with a new encoder, ended by {@link stop}.
 */
export class AudioInput {
	/** Called with each encoded frame. */
	onchunk: ((chunk: EncodedAudioChunk) => void) | undefined;

	readonly #context: AudioContext;
	// The worklet node, once its module has loaded.
	readonly #node: Promise<AudioWorkletNode>;
	#capture: Capture | undefined;
	#closed = false;

	constructor() {
		this.#context = new AudioContext({ sampleRate: SAMPLE_RATE });
		this.#node = this.#context.audioWorklet.addModule(String(workletUrl)).then(() => {
			const options: CaptureOptions = { channels: CHANNELS };
			const node = new AudioWorkletNode(this.#context, PROCESSOR_NAME, {
				numberOfInputs: 1,
				numberOfOutputs: 1,
				// Whatever the source has is mixed to this many before the
				// processor sees it.
				channelCount: CHANNELS,
				channelCountMode: "explicit",
				processorOptions: options,
			});
			node.port.onmessage = ({ data }: MessageEvent<CaptureBlock>) => this.#encode(data);
			return node;
		});
		// reason: start() awaits this and reports the failure; without a
		// handler here a load that fails before any start is an unhandled rejection.
		this.#node.catch(() => {});
	}

	/** Samples per second of what is encoded. */
	get sampleRate(): number {
		return this.#context.sampleRate;
	}

	/** Channels of what is encoded. */
	get channels(): number {
		return CHANNELS;
	}

	/**
	 * Begins capturing `stream`'s audio. Browsers hold an AudioContext
	 * suspended until a user gesture, so this must be reached from one.
	 *
	 * @param config - The encoder config, for {@link sampleRate} and {@link channels}.
	 * @param clock - The run's clock, in microseconds; timestamps start from it.
	 */
	async start(
		stream: MediaStream,
		config: AudioEncoderConfig,
		clock: () => number,
	): Promise<void> {
		this.stop();
		const encoder = new AudioEncoder({
			output: (chunk) => this.onchunk?.(chunk),
			error: (err) => log.error("audio encoder error", { err }),
		});
		encoder.configure(config);
		const capture: Capture = {
			encoder,
			source: this.#context.createMediaStreamSource(stream),
			rebase: new Rebase(),
			clock,
			samples: 0,
		};
		this.#capture = capture;

		try {
			const node = await this.#node;
			await this.#context.resume();
			// Stopped, or started again, while the worklet loaded.
			if (this.#capture !== capture) return;

			const reset: CaptureMessage = { type: "reset" };
			node.port.postMessage(reset);
			capture.source.connect(node);
		} catch (err) {
			if (this.#capture === capture) this.stop();
			throw err;
		}
	}

	/** Ends the capture. Nothing is encoded until the next {@link start}. */
	stop(): void {
		const capture = this.#capture;
		if (capture === undefined) return;
		this.#capture = undefined;
		capture.source.disconnect();
		if (capture.encoder.state !== "closed") capture.encoder.close();
		if (this.#closed) return;
		// reason: suspend only rejects on a closed context.
		this.#context.suspend().catch(() => {});
	}

	close(): void {
		if (this.#closed) return;
		this.#closed = true;
		this.stop();
		// reason: close only rejects on a context that is already closed.
		this.#context.close().catch(() => {});
	}

	#encode(block: CaptureBlock): void {
		const capture = this.#capture;
		if (capture === undefined || capture.encoder.state !== "configured") return;

		const frames = block.length / CHANNELS;
		// The block's first sample was captured a block's length before the
		// block got here.
		const timestamp = capture.rebase.at(
			capture.samples * 1_000_000 / this.sampleRate,
			capture.clock() - BLOCK_MS * 1000,
		);
		// Counted whether or not the block is encoded, so that a dropped
		// block leaves its gap in the timestamps.
		capture.samples += frames;

		if (capture.encoder.encodeQueueSize > MAX_ENCODE_QUEUE) return;

		const data = new AudioData({
			format: "f32-planar",
			sampleRate: this.sampleRate,
			numberOfChannels: CHANNELS,
			numberOfFrames: frames,
			timestamp,
			data: block,
		});
		try {
			capture.encoder.encode(data);
		} finally {
			data.close();
		}
	}
}
