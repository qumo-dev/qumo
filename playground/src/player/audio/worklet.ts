// Runs on the audio rendering thread: plays what the main thread decodes,
// through the ring buffer. Loaded with audioWorklet.addModule.
import { AudioRing, type AudioRingStats } from "./ring.ts";

/** The name this processor is registered under. */
export const PROCESSOR_NAME = "qumo-audio-output";

/** What the main thread sends the processor. */
export type WorkletMessage =
	/** A decoded block; `timestamp` is the media time of its first sample, in microseconds. */
	| { type: "write"; timestamp: number; channels: Float32Array[] }
	/** A new latency, in milliseconds. */
	| { type: "latency"; latency: number }
	/** A new stream follows: forget what is buffered, and take this latency. */
	| { type: "reset"; latency: number };

/** What the processor reports back, several times a second. */
export interface WorkletReport extends AudioRingStats {
	/** The least that was buffered since the previous report, in samples. */
	low: number;
	/** Samples per second, to turn the sample counts into time. */
	rate: number;
}

/** What the processor is created with, as `processorOptions`. */
export interface WorkletOptions {
	/** Milliseconds of audio to hold before playing. */
	latency: number;
}

// The options reach the processor from the page by structured clone, untyped.
function latencyOf(options: unknown): number {
	if (
		typeof options === "object" && options !== null && "latency" in options &&
		typeof options.latency === "number"
	) {
		return options.latency;
	}
	throw new TypeError("audio worklet: processorOptions.latency must be a number");
}

// The audio worklet's global scope, which the DOM typings do not describe.
declare const sampleRate: number;
declare class AudioWorkletProcessor {
	readonly port: MessagePort;
}
declare function registerProcessor(
	name: string,
	processor: new (options: AudioWorkletNodeOptions) => AudioWorkletProcessor,
): void;

// This module is also imported on the main thread, for the names above; the
// processor exists only where there is a worklet scope to register it in.
if (typeof registerProcessor === "function") {
	// How often the processor reports, in samples played: ten times a second.
	const REPORT_EVERY = Math.round(sampleRate / 10);

	class AudioOutputProcessor extends AudioWorkletProcessor {
		readonly #ring: AudioRing;
		#sinceReport = 0;

		constructor(options: AudioWorkletNodeOptions) {
			super();
			const latency = latencyOf(options.processorOptions);
			this.#ring = new AudioRing({
				rate: sampleRate,
				channels: options.outputChannelCount?.[0] ?? 2,
				latency,
			});

			this.port.onmessage = ({ data }: MessageEvent<WorkletMessage>) => {
				if (data.type === "write") {
					this.#ring.write(data.timestamp, data.channels);
					return;
				}
				if (data.type === "reset") this.#ring.reset();
				this.#ring.resize(data.latency);
			};
		}

		process(_inputs: Float32Array[][], outputs: Float32Array[][]): boolean {
			const output = outputs[0];
			if (output !== undefined) this.#ring.read(output);

			this.#sinceReport += output?.[0]?.length ?? 0;
			if (this.#sinceReport >= REPORT_EVERY) {
				this.#sinceReport = 0;
				const report: WorkletReport = {
					...this.#ring.stats,
					low: this.#ring.takeLow(),
					rate: sampleRate,
				};
				this.port.postMessage(report);
			}
			// Keep running while idle: audio may start arriving at any time.
			return true;
		}
	}

	registerProcessor(PROCESSOR_NAME, AudioOutputProcessor);
}
