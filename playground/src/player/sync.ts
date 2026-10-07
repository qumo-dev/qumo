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

// The most delay worth adding to keep audio from running dry. Past this the
// stream is not arriving well enough for a longer wait to rescue it.
const MAX_DELAY_MS = 500;

/**
 * The delay to move to after the audio buffer has run dry at `delay`
 * (milliseconds): half as much again, up to a ceiling. Running dry means the
 * delay was shorter than the gaps in how the audio arrives, so a small step
 * would only run dry again.
 */
export function raisedDelay(delay: number): number {
	return Math.max(delay, Math.min(MAX_DELAY_MS, delay * 1.5));
}

// What is held on top of the measured jitter: a fifth more, for the arrival
// that is a little worse than any seen yet, and one audio frame, because a
// frame is only useful once all of it is there.
const JITTER_HEADROOM = 1.2;
const JITTER_FRAME_MS = 20;

/**
 * The delay, in milliseconds, that covers arrivals `jitter` milliseconds late:
 * the jitter with some headroom, plus the wait a late audio group is given,
 * never less than `floor` and never more than the ceiling.
 */
export function delayForJitter(jitter: number, floor: number): number {
	// The wait for a late audio group is spent out of the same buffer.
	const needed = jitter * JITTER_HEADROOM + JITTER_FRAME_MS + AUDIO_WAIT_MS;
	return Math.max(floor, Math.min(MAX_DELAY_MS, needed));
}

// Raising the delay holds the sound back while the buffer fills to it: a short
// silence every time. A raise smaller than this is not worth one, since the
// headroom already covers it; and a raise that is made goes this much further,
// so the next creep of the jitter does not ask for another.
const MIN_RAISE_MS = 10;

/**
 * The delay to play at when it is `current` and the arrivals call for
 * `wanted` (both milliseconds): unchanged unless the difference is worth the
 * interruption, and then a little past what was asked, up to the ceiling.
 */
export function steppedDelay(current: number, wanted: number): number {
	if (wanted - current < MIN_RAISE_MS) return current;
	return Math.max(current, Math.min(MAX_DELAY_MS, wanted + MIN_RAISE_MS));
}

/**
 * How long, in milliseconds, a missing or stalled group of `track` is worth
 * waiting for when playback trails the live edge by `delay`.
 *
 * While a group is waited for, nothing after it is played, so the wait is paid
 * for out of what is already buffered: the delay. Video can spend all of it,
 * since a video group is seconds long and giving one up is the worse outcome.
 * An audio group is a single frame: giving it up costs one frame of silence,
 * while waiting the full delay for it empties the buffer and costs far more.
 * Audio therefore waits about one frame, which is enough for groups that were
 * merely sent side by side to arrive in either order, and leaves the rest of
 * the delay as the cushion it was meant to be.
 */
export function waitBudget(track: "video" | "audio", delay: number): number {
	return track === "audio" ? Math.min(delay, AUDIO_WAIT_MS) : delay;
}

/** The longest an audio group is waited for, in milliseconds. */
export const AUDIO_WAIT_MS = 20;

export class Sync {
	/**
	 * How far playback trails the live edge, in milliseconds. It can be raised
	 * while playing; what is already scheduled keeps its time.
	 */
	delay: number;

	// Smallest (arrival - media time) seen in the current and the previous
	// window. The reference is the smaller of the two.
	#current: number | undefined;
	#previous: number | undefined;
	#windowStart: number | undefined;
	// The latest any arrival has been against the fastest before it, in the
	// same two windows.
	#jitterCurrent = 0;
	#jitterPrevious = 0;

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
			const stale = now - this.#windowStart >= 2 * WINDOW_MS;
			this.#previous = stale ? undefined : this.#current;
			this.#jitterPrevious = stale ? 0 : this.#jitterCurrent;
			this.#current = undefined;
			this.#jitterCurrent = 0;
			this.#windowStart = now;
		}

		const offset = now - timestamp / 1000;
		// Lateness is measured against the fastest arrival before this one,
		// not after: a backlog delivered in one burst arrives oldest first,
		// each frame faster than the last, and none of it is jitter.
		const reference = min(this.#current, this.#previous);
		if (reference !== undefined && offset - reference > this.#jitterCurrent) {
			this.#jitterCurrent = offset - reference;
		}
		if (this.#current === undefined || offset < this.#current) this.#current = offset;
	}

	/**
	 * How late, in milliseconds, the latest arrival of the recent past was
	 * against the fastest one before it. This is how much has to be buffered
	 * for playback not to run out: it covers the network, a source that sends
	 * in bursts, and time the arrival spent waiting to be read.
	 */
	get jitter(): number {
		return Math.max(this.#jitterCurrent, this.#jitterPrevious);
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
