import { createLogger, MediaTags } from "@okdaichi/media-log";
import { InputGaps } from "./gaps.ts";
import {
	PROCESSOR_NAME,
	type WorkletMessage,
	type WorkletOptions,
	type WorkletReport,
} from "./worklet.ts";
// reason: Vite resolves `?worker&url` to the URL of the bundled worklet. The
// type checker reads the file itself, which has no default export.
// @ts-expect-error TS1192
import workletUrl from "./worklet.ts?worker&url";

const log = createLogger(MediaTags.audio);

// The rate the output runs at. It has to match the source (the publisher and
// the ingests all produce 48 kHz): the ring places each block by its timestamp
// at this rate, so audio at another rate would be laid out at the wrong pitch.
const SAMPLE_RATE = 48000;

/** The state of the audio jitter buffer, in milliseconds unless noted. */
export interface AudioBufferStats {
	/** Audio buffered and not yet played. */
	buffered: number;
	/** The least that was buffered since the previous report. */
	low: number;
	/** How much is held before playing, and refilled to after running dry. */
	latency: number;
	/** True while playback is held back, waiting to (re)fill. */
	stalled: boolean;
	/** Times playback ran dry (a count). */
	underruns: number;
	/** Silence played because playback was held back or had run dry. */
	starved: number;
	/** Silence played where audio was missing. */
	gaps: number;
	/** Audio that arrived after its time had been played. */
	late: number;
	/** Audio given up, once playing, because more arrived than the buffer holds. */
	overflowed: number;
	/** Audio skipped because the buffer was holding more than it ever used. */
	trimmed: number;
	/** How far the media has been written to, on its own timeline. */
	written: number;
	/** How much the output has played, audio or silence. */
	played: number;
}

/**
 * Decodes encoded audio and plays it through a jitter buffer.
 *
 * It owns its AudioContext, decoder and worklet, and is made once and reused:
 * opening an output and loading its worklet takes long enough to hear, so it
 * is done ahead of the first playback, and {@link reset} readies it for each
 * one after.
 */
export class AudioOutput {
	/** Called with the jitter buffer's state, several times a second. */
	onstats: ((stats: AudioBufferStats) => void) | undefined;

	readonly #context: AudioContext;
	readonly #gain: GainNode;
	#decoder: AudioDecoder;
	readonly #gaps = new InputGaps();
	// The worklet node, once its module has loaded.
	#node: AudioWorkletNode | undefined;
	// The latency the worklet should have; it may be set before the worklet exists.
	#latency: number;
	#closed = false;

	/** @param latency - Milliseconds of audio to hold before playing. */
	constructor(latency: number) {
		this.#latency = latency;
		this.#context = new AudioContext({ sampleRate: SAMPLE_RATE });
		this.#gain = new GainNode(this.#context);
		this.#gain.connect(this.#context.destination);

		this.#decoder = this.#newDecoder();

		this.#context.audioWorklet.addModule(String(workletUrl)).then(() => {
			if (this.#closed) return;
			const options: WorkletOptions = { latency: this.#latency };
			this.#node = new AudioWorkletNode(this.#context, PROCESSOR_NAME, {
				numberOfInputs: 0,
				numberOfOutputs: 1,
				outputChannelCount: [this.#context.destination.channelCount],
				processorOptions: options,
			});
			this.#node.port.onmessage = ({ data }: MessageEvent<WorkletReport>) => {
				const ms = (samples: number) => samples * 1000 / data.rate;
				this.onstats?.({
					buffered: ms(data.buffered),
					low: ms(data.low),
					latency: ms(data.latency),
					stalled: data.stalled,
					underruns: data.underruns,
					starved: ms(data.starved),
					gaps: ms(data.gaps),
					late: ms(data.late),
					overflowed: ms(data.overflowed),
					trimmed: ms(data.trimmed),
					written: ms(data.written),
					played: ms(data.played),
				});
			};
			this.#node.connect(this.#gain);
		}).catch((err: unknown) => {
			// Closing the context while the module loads rejects the load.
			if (!this.#closed) log.error("audio worklet failed to load", { err });
		});
	}

	/** Playback level, 0 to 1. */
	set volume(value: number) {
		this.#gain.gain.value = value;
	}

	/**
	 * Starts the output clock. Browsers hold an AudioContext suspended until a
	 * user gesture, so this must be reached from one.
	 */
	resume(): Promise<void> {
		return this.#context.resume();
	}

	/** Stops the output clock. Nothing plays until {@link resume}. */
	suspend(): void {
		if (this.#closed) return;
		// reason: suspend only rejects on a closed context.
		this.#context.suspend().catch(() => {});
	}

	/**
	 * Readies the output for a new stream: drops what was being decoded and
	 * what was buffered, and holds `latency` milliseconds before playing.
	 * The decoder has to be configured again afterwards.
	 */
	reset(latency: number): void {
		if (this.#closed) return;
		// A decoder that hit an error closes itself and cannot be reused.
		if (this.#decoder.state === "closed") this.#decoder = this.#newDecoder();
		else this.#decoder.reset();
		this.#gaps.reset();
		this.#latency = latency;
		this.#post({ type: "reset", latency });
	}

	configure(config: AudioDecoderConfig): void {
		if (this.#closed) return;
		if (this.#decoder.state === "closed") {
			this.#decoder = this.#newDecoder();
			this.#gaps.reset();
		}
		this.#decoder.configure(config);
	}

	/** Changes how much audio is held before playing, in milliseconds. */
	setLatency(latency: number): void {
		this.#latency = latency;
		this.#post({ type: "latency", latency });
	}

	/** Decodes `chunk` and plays it. Dropped until the decoder is configured. */
	decode(chunk: EncodedAudioChunk): void {
		if (this.#closed || this.#decoder.state !== "configured") return;
		this.#gaps.fed(chunk.timestamp);
		this.#decoder.decode(chunk);
	}

	close(): void {
		if (this.#closed) return;
		this.#closed = true;
		if (this.#decoder.state !== "closed") this.#decoder.close();
		this.#node?.disconnect();
		// reason: close only rejects on a context that is already closed.
		this.#context.close().catch(() => {});
	}

	#newDecoder(): AudioDecoder {
		return new AudioDecoder({
			output: (data) => {
				try {
					this.#play(data);
				} finally {
					data.close();
				}
			},
			error: (err) => log.error("audio decoder error", { err }),
		});
	}

	#play(data: AudioData): void {
		const channels: Float32Array[] = [];
		for (let i = 0; i < data.numberOfChannels; i++) {
			const samples = new Float32Array(data.numberOfFrames);
			data.copyTo(samples, { format: "f32-planar", planeIndex: i });
			channels.push(samples);
		}
		// The decoder's own timestamps close up around any frame it was not
		// given; the ring needs the hole to stay where it was.
		const timestamp = this.#gaps.decoded(data.timestamp, data.duration);
		// Audio decoded before the worklet has loaded has nowhere to go.
		this.#post(
			{ type: "write", timestamp, channels },
			channels.map((c) => c.buffer),
		);
	}

	#post(message: WorkletMessage, transfer: Transferable[] = []): void {
		if (this.#closed) return;
		this.#node?.port.postMessage(message, transfer);
	}
}
