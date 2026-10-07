// Puts back the gaps an audio decoder takes out.
//
// A WebCodecs AudioDecoder stamps its output by counting samples from its first
// input: the timestamps of later inputs are not consulted. So when a frame is
// never fed to it (lost, or given up as too late), the decoded audio closes up
// around the hole and everything after is stamped one frame early. Played by
// timestamp, the audio then runs one frame ahead of where it should, and the
// buffer in front of the speaker is one frame shorter, for good. A few lost
// frames in, the buffer is gone.
//
// This remembers the timestamp of each chunk that goes into the decoder and
// compares it with the timestamp on the block that comes out. Where the two
// have come apart by half a block or more, time was taken out (or, on a
// timeline that jumped back, put in), and it is added back from there on.

export class InputGaps {
	// The timestamps of the chunks in the decoder, oldest first.
	readonly #pending: number[] = [];
	// What to add to the decoder's timestamps, in microseconds.
	#offset = 0;

	/** Call with each chunk's timestamp (microseconds) as it goes to the decoder. */
	fed(timestamp: number): void {
		this.#pending.push(timestamp);
	}

	/**
	 * Call with each decoded block's timestamp and duration (microseconds), in
	 * the order the decoder emits them. Returns the timestamp the block has on
	 * the media's own timeline.
	 */
	decoded(timestamp: number, duration: number): number {
		// One block comes out for each chunk that went in.
		const fed = this.#pending.shift();
		// Timestamps are rounded on their way here and the decoder's are
		// exact to the sample, so its own are kept until they are off by
		// half a block or more.
		if (fed !== undefined && Math.abs(fed - (timestamp + this.#offset)) >= duration / 2) {
			this.#offset = fed - timestamp;
		}
		return timestamp + this.#offset;
	}

	/** Forgets everything, for a decoder that has been reset. */
	reset(): void {
		this.#offset = 0;
		this.#pending.length = 0;
	}
}
