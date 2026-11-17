/**
 * Chooses which rendition of a live video to play, from how its frames arrive.
 *
 * A live stream is sent no faster than it is made, so the rate frames arrive
 * at says what the stream costs, not what the link could carry. What does show
 * the link is lateness. While the link keeps up, every frame arrives about as
 * long after its media time as the fastest one did; once it cannot, frames
 * fall further behind with each one sent. And, falling behind, they arrive as
 * fast as the link carries them: the rate they arrive at then is the link's.
 *
 * So the controller steps down when frames fall behind, or a group is lost, to
 * the best rendition the measured rate carries with room to spare. It steps
 * up one rendition at a time after a stretch with nothing late, and waits
 * twice as long to try again whenever a step up has to be taken back. The
 * start of each subscription measures the link outright: a relay sends the
 * newest group from its start, as fast as the link carries it, and a burst
 * like that too slow for the next rendition up rules the step out.
 */

/** A rendition as the controller weighs it. */
export interface Choice {
	/** Bits per second; undefined when not known, which counts as costly. */
	readonly bitrate: number | undefined;
}

/** A change of rendition the controller asks for. */
export interface Decision {
	/** Index of the rendition to play, into the renditions, tallest first. */
	readonly to: number;
	/** Why, for the log. */
	readonly reason: string;
}

/** What the controller sees, for display. */
export interface AbrStats {
	/** How far the newest frame arrived behind the fastest recent one, in ms. */
	readonly lag: number;
	/** The rate frames arrived at over the last window, in bits per second. */
	readonly receiveRate: number;
	/** The rate of the latest burst, in bits per second, if one was measured. */
	readonly burstRate: number | undefined;
}

export interface ControllerOptions {
	/** A monotonic clock in milliseconds. Defaults to `performance.now()`. */
	now?: () => number;
}

// Lag past which the controller steps down, in ms.
const DOWN_LAG_MS = 500;
// Lag a stretch must stay under to count as calm, in ms.
const CALM_LAG_MS = 150;
// How long after a switch no decision is made, in ms.
const SETTLE_MS = 3_000;
// The first calm stretch before a step up, and the longest a step up is put
// off for, in ms.
const UP_AFTER_MS = 4_000;
const MAX_UP_AFTER_MS = 60_000;
// A step down this soon after a step up takes the step up back, in ms.
const FAIL_WINDOW_MS = 10_000;
// The share of a measured rate a rendition may use.
const HEADROOM = 0.8;

// The window lag and the receive rate are measured over, in ms.
const WINDOW_MS = 10_000;
const RATE_WINDOW_MS = 2_000;
// A burst: consecutive frames arriving in under this share of the media time
// between them, at least this many, over at least this long.
const BURST_SHARE = 0.25;
const BURST_FRAMES = 5;
const BURST_MIN_MS = 15;
// How long a burst measurement is trusted for, in ms.
const BURST_TTL_MS = 30_000;

interface Arrival {
	readonly at: number;
	/** Arrival time less media time, in ms. */
	readonly offset: number;
	readonly bytes: number;
}

export class AbrController {
	readonly #now: () => number;

	#choices: readonly Choice[];
	#arrivals: Arrival[] = [];
	// The run of frames arriving back to back, for a burst measurement.
	#run: { start: number; at: number; media: number; bytes: number; frames: number } | undefined;
	#burst: { rate: number; at: number } | undefined;
	#switchedAt: number;
	#upAt: number | undefined; // when the last step up was made
	#calmSince: number;
	// When frames started arriving behind; undefined while they keep up.
	#lateSince: number | undefined;
	#upAfter: number;
	#lost = false; // a group was lost since the last decision

	constructor(choices: readonly Choice[], options: ControllerOptions = {}) {
		this.#choices = choices;
		this.#now = options.now ?? (() => performance.now());
		this.#upAfter = UP_AFTER_MS;
		this.#switchedAt = this.#now();
		this.#calmSince = this.#switchedAt;
	}

	/** Replaces the renditions, as a catalog update lists them. */
	set choices(choices: readonly Choice[]) {
		this.#choices = choices;
	}

	/** The rendition to start with: the cheapest, so playback starts soonest. */
	initial(): number {
		return Math.max(0, this.#choices.length - 1);
	}

	/** A frame of the rendition being played arrived. */
	frameArrived(timestamp: number, bytes: number): void {
		const at = this.#now();
		const media = timestamp / 1000;
		this.#arrivals.push({ at, offset: at - media, bytes });
		while (this.#arrivals.length > 0 && this.#arrivals[0].at < at - WINDOW_MS) {
			this.#arrivals.shift();
		}
		if (this.lag() >= CALM_LAG_MS) {
			this.#calmSince = at;
			this.#lateSince ??= at;
		} else {
			this.#lateSince = undefined;
		}
		this.#measureBurst(at, media, bytes);
	}

	/** A group of the rendition being played was lost: skipped, or cut short. */
	groupLost(): void {
		this.#lost = true;
		this.#calmSince = this.#now();
	}

	/**
	 * Playback moved from rendition `from` to `to`. `manual` says the viewer's
	 * owner chose it rather than the controller: a step down by hand says
	 * nothing about whether the last step up held.
	 */
	switched(from: number, to: number, manual = false): void {
		const now = this.#now();
		if (
			!manual && to > from && this.#upAt !== undefined && now - this.#upAt < FAIL_WINDOW_MS
		) {
			// The step up did not hold: try the next one later.
			this.#upAfter = Math.min(this.#upAfter * 2, MAX_UP_AFTER_MS);
		}
		this.#upAt = !manual && to < from ? now : undefined;
		this.#switchedAt = now;
		this.#calmSince = now;
		this.#lost = false;
		// Frames of the old rendition, still arriving, say nothing of the new.
		this.#arrivals = [];
		this.#run = undefined;
		this.#lateSince = undefined;
	}

	/** How far the newest frame arrived behind the fastest recent one, in ms. */
	lag(): number {
		const n = this.#arrivals.length;
		if (n === 0) return 0;
		let min = Infinity;
		for (const a of this.#arrivals) min = Math.min(min, a.offset);
		return this.#arrivals[n - 1].offset - min;
	}

	/**
	 * The rate frames arrived at over the last couple of seconds, or since
	 * `since` when that is later, in bit/s.
	 */
	receiveRate(since?: number): number {
		const now = this.#now();
		const from = Math.max(now - RATE_WINDOW_MS, since ?? -Infinity);
		// The first frame in the window marks where it starts: its bytes
		// arrived before that, and are not counted.
		let bytes = 0;
		let first: number | undefined;
		for (const a of this.#arrivals) {
			if (a.at < from) continue;
			if (first === undefined) {
				first = a.at;
				continue;
			}
			bytes += a.bytes;
		}
		const span = Math.max(now - (first ?? now), 250);
		return bytes * 8 * 1000 / span;
	}

	stats(): AbrStats {
		const burst = this.#burst && this.#now() - this.#burst.at < BURST_TTL_MS
			? this.#burst.rate
			: undefined;
		return { lag: this.lag(), receiveRate: this.receiveRate(), burstRate: burst };
	}

	/**
	 * Decides whether to leave rendition `current`, an index into the
	 * renditions, tallest first. Returns undefined to stay.
	 */
	decide(current: number): Decision | undefined {
		const now = this.#now();
		if (now - this.#switchedAt < SETTLE_MS) return undefined;
		const lowest = this.#choices.length - 1;

		const lag = this.lag();
		if ((lag > DOWN_LAG_MS || this.#lost) && current < lowest) {
			const why = this.#lost
				? "a group was lost"
				: `frames arrive ${Math.round(lag)} ms late`;
			this.#lost = false;
			// Behind, frames arrive as fast as the link carries them: the
			// rate since they fell behind is the link's, where one over a
			// window reaching back before that would credit it with more.
			const fits = this.#best(this.receiveRate(this.#lateSince) * HEADROOM);
			return { to: Math.max(current + 1, fits), reason: why };
		}
		this.#lost = false;

		if (this.#upAt !== undefined && now - this.#upAt >= FAIL_WINDOW_MS) {
			// The last step up held: the next may come as soon as the first.
			this.#upAfter = UP_AFTER_MS;
			this.#upAt = undefined;
		}
		if (current > 0 && now - this.#calmSince >= this.#upAfter) {
			const next = this.#choices[current - 1].bitrate;
			const burst = this.stats().burstRate;
			if (next !== undefined && burst !== undefined && burst * HEADROOM < next) {
				return undefined; // the link measured too slow for it
			}
			return {
				to: current - 1,
				reason: `calm for ${Math.round((now - this.#calmSince) / 1000)} s`,
			};
		}
		return undefined;
	}

	// The tallest rendition whose bitrate fits within rate; the lowest if none does.
	#best(rate: number): number {
		const i = this.#choices.findIndex((c) => c.bitrate !== undefined && c.bitrate <= rate);
		return i === -1 ? this.#choices.length - 1 : i;
	}

	#measureBurst(at: number, media: number, bytes: number): void {
		const run = this.#run;
		if (
			run !== undefined && media > run.media &&
			at - run.at < (media - run.media) * BURST_SHARE
		) {
			run.at = at;
			run.media = media;
			run.bytes += bytes;
			run.frames++;
			return;
		}
		if (run !== undefined && run.frames >= BURST_FRAMES && run.at - run.start >= BURST_MIN_MS) {
			// The first frame's bytes arrived before the run's clock started.
			const rate = run.bytes * 8 * 1000 / (run.at - run.start);
			this.#burst = { rate, at: run.at };
		}
		this.#run = { start: at, at, media, bytes: 0, frames: 1 };
	}
}
