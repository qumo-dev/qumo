// Sends one track's encoded frames to every subscriber of it, as groups.
// It knows nothing of MoQ or of the frames' format: a subscriber is anything
// that opens groups, and a frame is whatever those groups take.

/** An open group of one subscriber. */
export interface GroupSink<F> {
	readonly sequence: number;
	writeFrame(frame: F): Promise<Error | undefined>;
	close(): Promise<void>;
}

/** One subscriber's end of a track. */
export interface TrackSink<F> {
	openGroup(): Promise<readonly [GroupSink<F>, undefined] | readonly [undefined, Error]>;
}

/** Told what is sent, group by group. A track is named per subscriber. */
export interface SendObserver {
	/** `track` is one this side sends, not one it plays. */
	markSent(track: string): void;
	groupArrived(track: string, group: number): void;
	frameArrived(track: string, group: number, timestamp: number, bytes: number): void;
	groupEnded(track: string, group: number, outcome: "complete" | "aborted" | "stopped"): void;
}

/** What the fan-out needs to know of a frame. */
export interface FrameInfo {
	/** Whether the frame can be decoded without the ones before it. */
	key: boolean;
	/** The frame's timestamp, in microseconds. */
	timestamp: number;
	/** The frame's encoded size. */
	bytes: number;
}

/**
 * How frames are divided into groups.
 * - `keyframe`: a keyframe begins a group, and the frames up to the next one
 *   follow in it. For video, where a subscriber has to start at a keyframe.
 * - `frame`: every frame is a group by itself. For audio, where any frame
 *   can be decoded alone, so a subscriber can start anywhere and a late
 *   frame holds up nothing after it.
 */
export type Grouping = "keyframe" | "frame";

export interface FanoutOptions {
	grouping: Grouping;
	observer?: SendObserver;
	/** Called when a frame could not be sent to the subscriber named. */
	onerror?: (track: string, err: Error) => void;
	/**
	 * Frames a subscriber may have waiting to be written. One that falls
	 * further behind has frames dropped, up to the next keyframe.
	 */
	maxPending?: number;
}

// About two seconds of video, or a second and a half of audio: a subscriber
// this far behind is not going to catch up by being sent more.
const MAX_PENDING = 64;

interface Subscriber<F> {
	readonly name: string;
	readonly sink: TrackSink<F>;
	// The group frames are being written to.
	group: GroupSink<F> | undefined;
	// The last frame's write. Writes go one after another, so that frames
	// keep their order and a group is open before anything is written to it.
	tail: Promise<void>;
	pending: number;
	// Set when a frame was dropped: what follows it cannot be decoded.
	needsKey: boolean;
	// Set when a frame was dropped from the open group, which is then not whole.
	torn: boolean;
	removed: boolean;
}

/** Sends a track's frames to each of its subscribers. */
export class Fanout<F> {
	readonly #grouping: Grouping;
	readonly #observer: SendObserver | undefined;
	readonly #onerror: ((track: string, err: Error) => void) | undefined;
	readonly #maxPending: number;
	readonly #subscribers = new Map<TrackSink<F>, Subscriber<F>>();

	constructor(options: FanoutOptions) {
		this.#grouping = options.grouping;
		this.#observer = options.observer;
		this.#onerror = options.onerror;
		this.#maxPending = options.maxPending ?? MAX_PENDING;
	}

	/** How many subscribers there are. */
	get size(): number {
		return this.#subscribers.size;
	}

	/**
	 * Adds a subscriber. It is sent nothing until the next keyframe.
	 *
	 * @param name - What the observer is told this subscriber's track is called.
	 */
	add(name: string, sink: TrackSink<F>): void {
		this.#subscribers.set(sink, {
			name,
			sink,
			group: undefined,
			tail: Promise.resolve(),
			pending: 0,
			needsKey: false,
			torn: false,
			removed: false,
		});
		this.#observer?.markSent(name);
	}

	/** Removes a subscriber. Its open group, if any, is reported as stopped. */
	remove(sink: TrackSink<F>): void {
		const subscriber = this.#subscribers.get(sink);
		if (subscriber === undefined) return;
		this.#subscribers.delete(sink);
		subscriber.removed = true;
		if (subscriber.group !== undefined) {
			this.#observer?.groupEnded(subscriber.name, subscriber.group.sequence, "stopped");
			subscriber.group = undefined;
		}
	}

	/**
	 * Sends a frame to every subscriber. A frame sent while there are none
	 * is dropped: the stream is live, and a subscriber starts at the next
	 * keyframe after it arrives.
	 *
	 * @returns A promise that resolves when each subscriber has been written
	 *   to, or has failed; it never rejects.
	 */
	send(frame: F, info: FrameInfo): Promise<void> {
		const writes = [...this.#subscribers.values()].map((subscriber) => {
			if (subscriber.pending >= this.#maxPending) {
				if (!subscriber.needsKey) {
					subscriber.needsKey = true;
					// The group this frame belonged to is the one open once
					// the frames ahead of it have been written.
					subscriber.tail = subscriber.tail.then(() => {
						subscriber.torn = true;
					});
				}
				return subscriber.tail;
			}
			if (subscriber.needsKey && !info.key) return subscriber.tail;
			subscriber.needsKey = false;

			subscriber.pending++;
			subscriber.tail = subscriber.tail
				.then(() => this.#write(subscriber, frame, info))
				.catch((err: unknown) => {
					this.#onerror?.(
						subscriber.name,
						err instanceof Error ? err : new Error(String(err)),
					);
				})
				.finally(() => {
					subscriber.pending--;
				});
			return subscriber.tail;
		});
		return Promise.all(writes).then(() => {});
	}

	async #write(subscriber: Subscriber<F>, frame: F, info: FrameInfo): Promise<void> {
		if (subscriber.removed) return;
		const { name } = subscriber;

		if (info.key) {
			this.#end(subscriber, subscriber.torn ? "aborted" : "complete");
			subscriber.torn = false;
			const [opened, err] = await subscriber.sink.openGroup();
			if (err !== undefined) throw err;
			if (subscriber.removed) {
				void opened.close();
				return;
			}
			subscriber.group = opened;
			this.#observer?.groupArrived(name, opened.sequence);
		}

		// A subscriber that arrived mid-group, or whose group failed, waits
		// for the next keyframe.
		const group = subscriber.group;
		if (group === undefined) return;

		const err = await group.writeFrame(frame);
		if (err !== undefined) {
			// Nothing more of this group can be sent.
			if (subscriber.group === group) this.#end(subscriber, "aborted");
			throw err;
		}
		this.#observer?.frameArrived(name, group.sequence, info.timestamp, info.bytes);

		if (this.#grouping === "frame" && subscriber.group === group) {
			this.#end(subscriber, "complete");
		}
	}

	// Closes the subscriber's open group, if it has one.
	#end(subscriber: Subscriber<F>, outcome: "complete" | "aborted"): void {
		const group = subscriber.group;
		if (group === undefined) return;
		subscriber.group = undefined;
		// reason: the group is finished either way; a failed close is the
		// transport's to report, on the write that follows.
		group.close().catch(() => {});
		this.#observer?.groupEnded(subscriber.name, group.sequence, outcome);
	}
}
