import { VideoNode } from "@okdaichi/av-nodes";
import { Pacer } from "./pacer.ts";

/**
 * Passes video frames through after holding each one until it is due.
 *
 * Sits between a decode node and the destination: the decoder emits frames as
 * fast as it decodes them, and this spaces them back out on the playback clock.
 */
export class VideoPaceNode extends VideoNode {
	/**
	 * How long to hold a frame, in milliseconds, given its timestamp
	 * (microseconds). Unset, or a non-positive result, passes the frame at once.
	 */
	hold: ((timestamp: number) => number) | undefined;

	/** Called with a frame's timestamp (microseconds) as it is passed on. */
	onpresent: ((timestamp: number) => void) | undefined;

	readonly #pacer = new Pacer<VideoFrame>(
		(frame) => this.#emit(frame),
		(frame) => frame.close(),
	);

	constructor() {
		super({ numberOfInputs: 1, numberOfOutputs: 1 });
	}

	process(input?: VideoFrame): void {
		if (this.disposed || input === undefined) return;
		// The caller owns `input` and closes it on return, so hold a clone.
		this.#pacer.push(input.clone(), this.hold?.(input.timestamp) ?? 0);
	}

	/** Discards the frames still being held. */
	flush(): void {
		this.#pacer.clear();
	}

	override dispose(): void {
		this.#pacer.clear();
		super.dispose();
	}

	#emit(frame: VideoFrame): void {
		try {
			for (const output of this.outputs) {
				output.process(frame);
			}
			this.onpresent?.(frame.timestamp);
		} finally {
			frame.close();
		}
	}
}
