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

/**
 * Keeps a source that runs faster than the wanted frame rate down to it.
 *
 * It holds to a schedule, one frame an interval, and takes the frame nearest
 * each slot. Judging each frame by its distance from the last one taken
 * would not do: a source at the wanted rate rarely delivers evenly (a canvas
 * capture at 30 fps comes 17, 33 and 50 ms apart), and every frame that
 * followed another closely would be dropped though the rate was right.
 */
export class FrameLimiter {
	// How early a frame may be for its slot, as a share of the interval. Under
	// a half, so that a source at exactly twice the rate loses every other
	// frame and not a pattern that depends on rounding.
	static readonly #EARLY = 0.4;

	readonly #interval: number;
	// When the next frame is due, on the frames' own timeline.
	#due: number | undefined;

	/** @param framerate - Frames per second to let through, at most. */
	constructor(framerate: number) {
		this.#interval = 1_000_000 / framerate;
	}

	/** Whether to take the frame with this timestamp (microseconds). */
	takes(timestamp: number): boolean {
		const due = this.#due ?? timestamp;
		// Too early: the slot belongs to a later frame.
		if (timestamp < due - this.#interval * FrameLimiter.#EARLY) return false;
		// The schedule moves on by one slot. A source that missed a whole
		// slot starts it afresh, and does not get to make up with a burst.
		this.#due = (timestamp - due > this.#interval ? timestamp : due) + this.#interval;
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
