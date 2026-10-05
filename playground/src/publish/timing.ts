// The publisher's rules about time, free of the browser so they can be tested:
// which frames to encode, which of them to make keyframes, and what timestamp
// each carries.

/**
 * Puts a source's timestamps on the run's own timeline.
 *
 * A camera stamps its frames from wherever its clock happens to stand, and
 * the microphone's samples are counted from zero. Both are moved so that the
 * first one lands where the run's clock stood when it arrived; after that the
 * source's own spacing is kept, which is steadier than arrival times.
 */
export class Rebase {
	#offset: number | undefined;
	#last = -1;

	/**
	 * @param timestamp - The source's timestamp, in microseconds.
	 * @param now - The run's clock as this arrives, in microseconds.
	 * @returns The timestamp on the run's timeline; never at or before the last.
	 */
	at(timestamp: number, now: number): number {
		this.#offset ??= now - timestamp;
		// A source whose clock steps back would otherwise send time backwards.
		this.#last = Math.max(Math.round(timestamp + this.#offset), this.#last + 1);
		return this.#last;
	}
}

// How much sooner than the frame interval a frame may come and still be taken.
// A camera asked for 30 fps delivers frames a few milliseconds either side of
// 33 ms apart; one delivering 60 fps is 17 ms apart, and every other is dropped.
const EARLY = 0.75;

/** Keeps a source that runs faster than the wanted frame rate down to it. */
export class FrameLimiter {
	readonly #interval: number;
	#last: number | undefined;

	/** @param framerate - Frames per second to let through, at most. */
	constructor(framerate: number) {
		this.#interval = 1_000_000 / framerate;
	}

	/** Whether to take the frame with this timestamp (microseconds). */
	takes(timestamp: number): boolean {
		if (this.#last !== undefined && timestamp - this.#last < this.#interval * EARLY) {
			return false;
		}
		this.#last = timestamp;
		return true;
	}
}

/** Decides which frames are keyframes: the first, then one every `interval`. */
export class KeyframeCadence {
	readonly #interval: number;
	#last: number | undefined;

	/** @param interval - Microseconds between keyframes. */
	constructor(interval: number) {
		this.#interval = interval;
	}

	/** Whether the frame with this timestamp (microseconds) is to be a keyframe. */
	isKey(timestamp: number): boolean {
		if (this.#last !== undefined && timestamp - this.#last < this.#interval) return false;
		this.#last = timestamp;
		return true;
	}
}
