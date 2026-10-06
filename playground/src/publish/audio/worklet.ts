// Runs on the audio rendering thread: hands the page what the microphone (or
// a shared tab) produces, in blocks. Loaded with audioWorklet.addModule.
import { Blocks } from "./blocks.ts";

/** The name this processor is registered under. */
export const PROCESSOR_NAME = "qumo-audio-capture";

/** The length of the blocks the processor sends, in milliseconds. */
export const BLOCK_MS = 20;

/** What the page sends the processor: a new capture begins. */
export type CaptureMessage = { type: "reset" };

/**
 * What the processor sends the page: one block, every channel's samples one
 * channel after another. Its length is the channel count times the samples
 * of {@link BLOCK_MS}.
 */
export type CaptureBlock = Float32Array<ArrayBuffer>;

/** What the processor is created with, as `processorOptions`. */
export interface CaptureOptions {
	/** Channels in every block. */
	channels: number;
}

// The options reach the processor from the page by structured clone, untyped.
function channelsOf(options: unknown): number {
	if (
		typeof options === "object" && options !== null && "channels" in options &&
		typeof options.channels === "number"
	) {
		return options.channels;
	}
	throw new TypeError("audio capture worklet: processorOptions.channels must be a number");
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
	class AudioCaptureProcessor extends AudioWorkletProcessor {
		readonly #blocks: Blocks;

		constructor(options: AudioWorkletNodeOptions) {
			super();
			this.#blocks = new Blocks(
				channelsOf(options.processorOptions),
				Math.round(sampleRate * BLOCK_MS / 1000),
			);
			this.port.onmessage = (_: MessageEvent<CaptureMessage>) => this.#blocks.reset();
		}

		process(inputs: Float32Array[][]): boolean {
			// With nothing connected the input has no channels, and nothing is sent.
			this.#blocks.push(inputs[0] ?? [], (block: CaptureBlock) => {
				this.port.postMessage(block, [block.buffer]);
			});
			// Keep running while idle: a source may be connected at any time.
			return true;
		}
	}

	registerProcessor(PROCESSOR_NAME, AudioCaptureProcessor);
}
