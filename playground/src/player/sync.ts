// Playback timing for a track, after the moq-dev reference player's Sync: media
// is presented a fixed delay behind the fastest arrival seen. A broadcast's
// tracks are given the same delay, so they trail the live edge by the same
// amount, but each has its own clock: nothing requires their timestamps to
// start from the same origin.

// The reference adopts a new fastest arrival at once but lets go of an old
// one only after this long, so a single early frame does not jerk the clock.
const WINDOW_MS = 10_000;

// Smallest delay worth holding: below this the audio cushion underruns on
// sources that emit in bursts (RTSP/RTMP AAC arrives ~80 ms at a time), which
// a round-trip time cannot see. It stays the floor until the delay is measured
// from arrival timing instead.
const MIN_DELAY_MS = 100;

/**
 * The delay, in milliseconds, to play behind the live edge on a connection
 * with round-trip time `rtt` (milliseconds): room for one retransmit, and
 * never less than the floor.
 */
export function delayFor(rtt: number | undefined): number {
	if (rtt === undefined || !(rtt > 0)) return MIN_DELAY_MS;
	return Math.max(MIN_DELAY_MS, rtt * 1.25);
}

export class Sync {
	/** How far playback trails the live edge, in milliseconds. */
	readonly delay: number;

	// Smallest (arrival - media time) seen in the current and the previous
	// window. The reference is the smaller of the two.
	#current: number | undefined;
	#previous: number | undefined;
	#windowStart: number | undefined;

	constructor(delay: number) {
		this.delay = delay;
	}

	/**
	 * Records that media with `timestamp` (microseconds) arrived at `now`
	 * (milliseconds on a monotonic clock).
	 */
	observe(timestamp: number, now: number): void {
		if (this.#windowStart === undefined) {
			this.#windowStart = now;
		} else if (now - this.#windowStart >= WINDOW_MS) {
			// After a gap longer than two windows the previous one is stale too.
			this.#previous = now - this.#windowStart >= 2 * WINDOW_MS ? undefined : this.#current;
			this.#current = undefined;
			this.#windowStart = now;
		}

		const offset = now - timestamp / 1000;
		if (this.#current === undefined || offset < this.#current) this.#current = offset;
	}

	/**
	 * When media with `timestamp` (microseconds) is due, in milliseconds on the
	 * clock passed to {@link observe}. Undefined until something was observed.
	 */
	due(timestamp: number): number | undefined {
		const reference = min(this.#current, this.#previous);
		if (reference === undefined) return undefined;
		return reference + timestamp / 1000 + this.delay;
	}
}

function min(a: number | undefined, b: number | undefined): number | undefined {
	if (a === undefined) return b;
	if (b === undefined) return a;
	return Math.min(a, b);
}
