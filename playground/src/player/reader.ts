import type { Group, Track } from "./source.ts";

/**
 * Splits a frame's bytes into its media timestamp (microseconds) and encoded
 * data. `data` must be a copy: `bytes` is reused for the next frame.
 */
export type Unpack = (bytes: Uint8Array) => { timestamp: number; data: Uint8Array };

/** One frame of a track, tagged with where it sits in the stream. */
export interface TrackFrame {
	/** Sequence of the group the frame belongs to. */
	readonly group: number;
	/** Position of the frame within its group, from 0. */
	readonly index: number;
	/** Media timestamp in microseconds. */
	readonly timestamp: number;
	readonly data: Uint8Array;
}

/** How a group's reading ended. */
export type GroupOutcome =
	/** Read to its end. */
	| "complete"
	/** Ended by the sender or the network before its end. */
	| "aborted"
	/** Given up on by the reader: waiting for it had cost more than maxAge. */
	| "skipped"
	/** Arrived after delivery had passed it, and was never read. */
	| "late"
	/** Still being read when the reader itself was stopped. */
	| "stopped";

/** Told what happens to a track's groups as they come off the transport. */
export interface GroupObserver {
	groupArrived(group: number): void;
	/** A frame of `bytes` encoded bytes was read, before any decision to skip. */
	frameArrived(group: number, timestamp: number, bytes: number): void;
	groupEnded(group: number, outcome: GroupOutcome): void;
}

export interface ReadOptions {
	unpack: Unpack;
	observer?: GroupObserver;
	/**
	 * How long, in milliseconds, a missing or stalled group is waited for once
	 * a later group has a frame ready. The wait also has to be worth ending:
	 * the later media must be more than this far ahead, in media time, of the
	 * last delivered frame. 0 skips as soon as any newer frame exists.
	 */
	maxAge?: number;
	/** A monotonic clock in milliseconds. Defaults to `performance.now()`. */
	now?: () => number;
	/**
	 * How late for playback, in milliseconds, a frame with this timestamp
	 * (microseconds) would be if delivered now; negative when early. With it,
	 * a group that could only be played more than `maxAge` late is skipped
	 * whole when a newer one is ready, so playback returns to the live edge
	 * after a stall instead of working through the backlog.
	 */
	lateness?: (timestamp: number) => number;
}

// Sized for one retransmit on a typical connection; the same fallback the
// moq-dev reference player uses when it has no RTT estimate.
const DEFAULT_MAX_AGE_MS = 100;

/** Thrown by {@link readFrames} when the track has no more frames to give. */
export class TrackEndedError extends Error {
	constructor(cause: Error) {
		super("track ended", { cause });
		this.name = "TrackEndedError";
	}
}

/**
 * Yields a track's frames in group order, then frame order.
 *
 * Groups are accepted and buffered as they arrive, so a slow group does not
 * hold back the ones behind it on the wire. Delivery still waits for the next
 * group in sequence, but only for `maxAge`: once a later group has had a frame
 * ready for that long, and its media is further ahead than that, the group
 * being waited on is cancelled and delivery moves on. A group arriving after
 * delivery has passed it is cancelled at once.
 *
 * Two more moves keep delivery close to the live edge. A group whose frames
 * have all been delivered, and which the next group's first frame follows
 * without a gap, is finished: delivery moves on without waiting for its stream
 * to end. And, given `lateness`, a group that is already too late when its
 * turn comes is skipped whole if a newer one is ready.
 *
 * The scheme follows the moq-dev reference player's container consumer, with
 * the wait measured on the clock as well as in media time: groups that reach a
 * new subscriber in one burst can span more media than `maxAge` while being
 * microseconds apart, and none of them is late.
 *
 * It never ends normally. It throws {@link TrackEndedError} when `done`
 * resolves, or once the track stops yielding groups and the buffered ones have
 * been delivered; it throws the `unpack` error if a frame cannot be unpacked.
 * A group aborted part-way is not an error: its frames so far are delivered.
 */
export async function* readFrames(
	track: Track,
	done: Promise<void>,
	options: ReadOptions,
): AsyncGenerator<TrackFrame, never> {
	const reader = new Reader(track, done, options);
	try {
		while (true) {
			yield await reader.next();
		}
	} finally {
		reader.close();
	}
}

interface Buffered {
	readonly group: Group;
	/** Frames read but not yet delivered, in order. */
	readonly frames: TrackFrame[];
	/** Timestamp of the first frame read from the group. */
	first: number | undefined;
	/** Newest timestamp read from the group so far. */
	latest: number | undefined;
	/** Media time between the last two frames read. */
	interval: number | undefined;
	/** When the group arrived or last produced a frame, on the reader's clock. */
	touched: number;
	/** When the group's first frame was read; undefined until then. */
	readyAt: number | undefined;
	/** The group's stream has ended, cleanly or not. */
	done: boolean;
	/** How the reader ended the group, once it has moved past it itself. */
	ending: GroupOutcome | undefined;
}

class Reader {
	readonly #track: Track;
	readonly #done: Promise<void>;
	readonly #unpack: Unpack;
	readonly #observer: GroupObserver | undefined;
	readonly #now: () => number;
	readonly #maxAgeMs: number;
	readonly #maxAge: number; // microseconds
	readonly #lateness: ((timestamp: number) => number) | undefined;

	// Groups being read, ascending by sequence. The head is the next to deliver.
	readonly #groups: Buffered[] = [];
	// Sequence of the group delivery is at, or waiting for.
	#active: number | undefined;
	// Timestamp of the last frame delivered.
	#delivered: number | undefined;
	// Why no further groups will be accepted, once that is the case.
	#ended: Error | undefined;
	#stopped = false;
	#failure: Error | undefined;
	#wake: (() => void) | undefined;
	#timer: number | undefined;

	constructor(track: Track, done: Promise<void>, options: ReadOptions) {
		this.#track = track;
		this.#done = done;
		this.#unpack = options.unpack;
		this.#observer = options.observer;
		this.#now = options.now ?? (() => performance.now());
		this.#maxAgeMs = options.maxAge ?? DEFAULT_MAX_AGE_MS;
		this.#maxAge = this.#maxAgeMs * 1000;
		this.#lateness = options.lateness;

		void done.then(() => {
			this.#stopped = true;
			this.#notify();
		});
		void this.#accept();
	}

	async next(): Promise<TrackFrame> {
		while (true) {
			if (this.#failure) throw this.#failure;
			if (this.#stopped) throw new TrackEndedError(this.#ended ?? new Error("stopped"));

			const retryAt = this.#skipAhead();
			if (retryAt === "moved") continue;

			const head = this.#groups[0];
			if (head !== undefined && head.group.sequence === this.#active) {
				if (this.#tooLate(head)) {
					this.#moveOn(head, "skipped");
					continue;
				}
				const frame = head.frames.shift();
				if (frame !== undefined) {
					this.#delivered = frame.timestamp;
					return frame;
				}
				if (head.done) {
					this.#groups.shift();
					this.#active = head.group.sequence + 1;
					continue;
				}
			}

			if (head === undefined && this.#ended) throw new TrackEndedError(this.#ended);

			await new Promise<void>((resolve) => {
				this.#wake = resolve;
				if (retryAt !== undefined) {
					const wait = Math.max(0, retryAt - this.#now());
					this.#timer = setTimeout(() => this.#notify(), wait);
				}
			});
		}
	}

	close(): void {
		this.#stopped = true;
		for (const buffered of this.#groups.splice(0)) {
			buffered.group.cancel();
		}
		this.#notify();
	}

	#notify(): void {
		if (this.#timer !== undefined) {
			clearTimeout(this.#timer);
			this.#timer = undefined;
		}
		const wake = this.#wake;
		this.#wake = undefined;
		wake?.();
	}

	async #accept(): Promise<void> {
		while (!this.#stopped) {
			let group: Group;
			try {
				const [accepted, err] = await this.#track.acceptGroup(this.#done);
				if (accepted === undefined) {
					this.#ended = err;
					break;
				}
				group = accepted;
			} catch (err) {
				this.#ended = toError(err);
				break;
			}

			// Delivery starts at the first group to arrive.
			this.#active ??= group.sequence;
			if (this.#stopped) {
				group.cancel();
				break;
			}
			this.#observer?.groupArrived(group.sequence);
			if (group.sequence < this.#active) {
				// Delivery has already passed this group.
				group.cancel();
				this.#observer?.groupEnded(group.sequence, "late");
				continue;
			}

			const buffered: Buffered = {
				group,
				frames: [],
				first: undefined,
				latest: undefined,
				interval: undefined,
				touched: this.#now(),
				readyAt: undefined,
				done: false,
				ending: undefined,
			};
			const at = this.#groups.findIndex((g) => g.group.sequence > group.sequence);
			this.#groups.splice(at === -1 ? this.#groups.length : at, 0, buffered);
			void this.#buffer(buffered);
		}
		this.#notify();
	}

	async #buffer(buffered: Buffered): Promise<void> {
		const sequence = buffered.group.sequence;
		let index = 0;
		let outcome: GroupOutcome = "complete";
		try {
			for await (const frame of buffered.group.frames()) {
				let unpacked: ReturnType<Unpack>;
				try {
					unpacked = this.#unpack(frame.bytes);
				} catch (err) {
					this.#failure = toError(err);
					return;
				}
				buffered.frames.push({ group: sequence, index, ...unpacked });
				index++;
				buffered.touched = this.#now();
				buffered.readyAt ??= buffered.touched;
				this.#observer?.frameArrived(
					sequence,
					unpacked.timestamp,
					unpacked.data.byteLength,
				);
				buffered.first ??= unpacked.timestamp;
				if (buffered.latest === undefined || unpacked.timestamp > buffered.latest) {
					if (buffered.latest !== undefined) {
						buffered.interval = unpacked.timestamp - buffered.latest;
					}
					buffered.latest = unpacked.timestamp;
				}
				this.#notify();
			}
		} catch {
			// reason: an aborted or cancelled group only loses its tail; the
			// frames read so far are still delivered and the next group follows.
			outcome = "aborted";
		} finally {
			buffered.done = true;
			// A stopped reader cancels every group still open, whatever state
			// it was in.
			if (this.#stopped) outcome = "stopped";
			else if (buffered.ending) outcome = buffered.ending;
			this.#observer?.groupEnded(sequence, outcome);
			this.#notify();
		}
	}

	// Moves delivery past a group that is missing or stalled, once it has been
	// waited for long enough. Returns "moved" if it did, or the time at which
	// the wait runs out and this is worth calling again.
	#skipAhead(): "moved" | number | undefined {
		const head = this.#groups[0];
		if (head === undefined || this.#active === undefined) return undefined;

		const missing = head.group.sequence > this.#active;
		if (missing && this.#ended) {
			// Once the track has ended the awaited group never will arrive.
			this.#active = head.group.sequence;
			return "moved";
		}
		if (!missing && (head.frames.length > 0 || head.done)) return undefined;
		if (!missing && this.#continuedBy(head, this.#groups[1])) {
			this.#moveOn(head, "complete");
			return "moved";
		}

		// What delivery would move on to: the first group past the awaited
		// one that has a frame.
		const ready = this.#groups.find((g, i) => (missing || i > 0) && g.readyAt !== undefined);
		if (ready?.readyAt === undefined || !this.#aheadOfDelivery()) return undefined;

		// The wait runs from when there was first something to move on to,
		// and starts over each time the awaited group produces a frame.
		const since = missing ? ready.readyAt : Math.max(ready.readyAt, head.touched);
		const until = since + this.#maxAgeMs;
		if (this.#now() < until) return until;

		if (missing) {
			this.#active = head.group.sequence;
			return "moved";
		}
		this.#moveOn(head, "skipped");
		return "moved";
	}

	// Ends the head group from this side and makes the next one the head.
	#moveOn(head: Buffered, ending: GroupOutcome): void {
		this.#groups.shift();
		head.ending = ending;
		head.group.cancel();
		this.#active = this.#groups[0]?.group.sequence ?? head.group.sequence + 1;
	}

	// Whether `head`, all of whose frames have been delivered, is carried on by
	// `next` without a gap: the next group's first frame comes one frame
	// interval after the last delivered one. A sender ends a group before it
	// starts the next, so nothing more of `head` is coming.
	#continuedBy(head: Buffered, next: Buffered | undefined): boolean {
		if (next?.first === undefined || head.interval === undefined) return false;
		if (this.#delivered === undefined || head.latest !== this.#delivered) return false;
		// Half an interval of slack for timestamps rounded on their way here.
		return next.first - this.#delivered <= head.interval * 1.5;
	}

	// Whether `head` has not been started yet and is already too late to play,
	// with something newer ready to play instead. Only a whole group is given
	// up this way: once its first frame is delivered the rest may be needed to
	// decode what follows.
	#tooLate(head: Buffered): boolean {
		const first = head.frames[0];
		if (this.#lateness === undefined || first === undefined || first.index !== 0) return false;
		if (!this.#groups.some((g, i) => i > 0 && g.readyAt !== undefined)) return false;
		return this.#lateness(first.timestamp) > this.#maxAgeMs;
	}

	// Whether buffered media has run further ahead of delivery than maxAge.
	#aheadOfDelivery(): boolean {
		let newest: number | undefined;
		for (const buffered of this.#groups) {
			if (buffered.latest === undefined) continue;
			if (newest === undefined || buffered.latest > newest) newest = buffered.latest;
		}
		if (newest === undefined) return false;
		// Nothing delivered yet, so there is no last frame to measure from:
		// measure from the oldest frame waiting instead. Groups reaching a new
		// subscriber together are then all played, and a first group that
		// never produces a frame is still given up on.
		return newest - (this.#delivered ?? this.#oldestBuffered() ?? newest) > this.#maxAge;
	}

	// Timestamp of the oldest frame read but not yet delivered.
	#oldestBuffered(): number | undefined {
		let oldest: number | undefined;
		for (const buffered of this.#groups) {
			const first = buffered.frames[0]?.timestamp;
			if (first === undefined) continue;
			if (oldest === undefined || first < oldest) oldest = first;
		}
		return oldest;
	}
}

function toError(value: unknown): Error {
	return value instanceof Error ? value : new Error(String(value));
}
