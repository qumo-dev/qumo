// Keeps a record of what happened to each track's groups, for the DevTools
// panel. It is fed by explicit calls and knows nothing of the player or of
// SolidJS: its method set happens to match the player's PlaybackObserver.
//
// A publisher records the tracks it sends through the same calls: a group
// "arrives" when it is opened and a frame "arrives" when it is written.

/** Where a group stands: still being read, or how its reading ended. */
export type GroupState = "receiving" | "complete" | "aborted" | "skipped" | "late" | "stopped";

/** How a group's reading can end: every state but "receiving". */
export type GroupOutcome = Exclude<GroupState, "receiving">;

export interface GroupRecord {
	readonly sequence: number;
	/** When the group arrived, in milliseconds on the recorder's clock. */
	readonly arrived: number;
	/** When the group ended; undefined while it is being received. */
	readonly ended: number | undefined;
	readonly state: GroupState;
	/** Frames and encoded bytes read from the group so far. */
	readonly frames: number;
	readonly bytes: number;
	/** How many of those frames reached their output. */
	readonly rendered: number;
	/** When the first and the latest of them did; undefined if none has. */
	readonly renderStart: number | undefined;
	readonly renderEnd: number | undefined;
}

export interface TrackRecord {
	readonly name: string;
	/** False for a track that is sent, which has nothing to render. */
	readonly renders: boolean;
	/** Groups still within the retention window, oldest first. */
	readonly groups: readonly GroupRecord[];
	/** Groups that ended in each way since recording began. */
	readonly ended: Readonly<Record<GroupOutcome, number>>;
	/** Frames and encoded bytes received, and frames rendered, since recording began. */
	readonly frames: number;
	readonly bytes: number;
	readonly rendered: number;
	/** Highest group sequence seen. */
	readonly latest: number | undefined;
}

const WINDOW_MS = 60_000;
// Backstop for a track whose groups never end.
const MAX_GROUPS = 4096;

type Mutable<T> = { -readonly [K in keyof T]: T[K] };

interface Track {
	renders: boolean;
	groups: Mutable<GroupRecord>[];
	bySequence: Map<number, Mutable<GroupRecord>>;
	// Which group each not-yet-rendered frame belongs to, by timestamp.
	byTimestamp: Map<number, Mutable<GroupRecord>>;
	ended: Record<GroupOutcome, number>;
	frames: number;
	bytes: number;
	rendered: number;
	latest: number | undefined;
}

export class Recorder {
	readonly #now: () => number;
	readonly #tracks = new Map<string, Track>();

	/** @param now - A monotonic clock in milliseconds. */
	constructor(now: () => number = () => performance.now()) {
		this.#now = now;
	}

	/** Marks `track` as one this side sends rather than plays. */
	markSent(track: string): void {
		this.#track(track).renders = false;
	}

	groupArrived(track: string, group: number): void {
		const t = this.#track(track);
		// snapshot() prunes, but nothing calls it while the panel is closed:
		// keep the record bounded here too.
		if (t.groups.length >= 2 * MAX_GROUPS) prune(t, this.#now() - WINDOW_MS);
		const record: Mutable<GroupRecord> = {
			sequence: group,
			arrived: this.#now(),
			ended: undefined,
			state: "receiving",
			frames: 0,
			bytes: 0,
			rendered: 0,
			renderStart: undefined,
			renderEnd: undefined,
		};
		t.groups.push(record);
		t.bySequence.set(group, record);
		if (t.latest === undefined || group > t.latest) t.latest = group;
	}

	frameArrived(track: string, group: number, timestamp: number, bytes: number): void {
		const t = this.#track(track);
		t.frames++;
		t.bytes += bytes;

		const record = t.bySequence.get(group);
		if (record === undefined) return;
		record.frames++;
		record.bytes += bytes;
		// A sent track renders nothing, so its frames are never looked up.
		if (t.renders) t.byTimestamp.set(timestamp, record);
	}

	groupEnded(track: string, group: number, outcome: GroupOutcome): void {
		const t = this.#track(track);
		const record = t.bySequence.get(group);
		// A group ends once; a second report (a write failing again on a
		// group that already failed) changes nothing.
		if (record !== undefined && record.state !== "receiving") return;
		t.ended[outcome]++;

		if (record === undefined) return;
		record.state = outcome;
		record.ended = this.#now();
	}

	frameRendered(track: string, timestamp: number): void {
		const t = this.#track(track);
		const record = t.byTimestamp.get(timestamp);
		if (record === undefined) return;
		t.byTimestamp.delete(timestamp);

		const now = this.#now();
		t.rendered++;
		record.rendered++;
		record.renderStart ??= now;
		record.renderEnd = now;
	}

	/** The tracks seen so far, in the order they first appeared. */
	snapshot(): TrackRecord[] {
		const cutoff = this.#now() - WINDOW_MS;
		const tracks: TrackRecord[] = [];
		for (const [name, t] of this.#tracks) {
			prune(t, cutoff);
			tracks.push({
				name,
				renders: t.renders,
				groups: t.groups.map((g) => ({ ...g })),
				ended: { ...t.ended },
				frames: t.frames,
				bytes: t.bytes,
				rendered: t.rendered,
				latest: t.latest,
			});
		}
		return tracks;
	}

	#track(name: string): Track {
		let t = this.#tracks.get(name);
		if (t === undefined) {
			t = {
				renders: true,
				groups: [],
				bySequence: new Map(),
				byTimestamp: new Map(),
				ended: { complete: 0, aborted: 0, skipped: 0, late: 0, stopped: 0 },
				frames: 0,
				bytes: 0,
				rendered: 0,
				latest: undefined,
			};
			this.#tracks.set(name, t);
		}
		return t;
	}
}

// Forgets groups that ended before `cutoff`, and the oldest beyond the cap.
function prune(t: Track, cutoff: number): void {
	const excess = t.groups.length - MAX_GROUPS;
	const kept = t.groups.filter((g, i) =>
		i >= excess && (g.ended === undefined || g.ended >= cutoff)
	);
	if (kept.length === t.groups.length) return;

	const live = new Set(kept);
	t.groups = kept;
	for (const [sequence, g] of t.bySequence) {
		if (!live.has(g)) t.bySequence.delete(sequence);
	}
	for (const [timestamp, g] of t.byTimestamp) {
		if (!live.has(g)) t.byTimestamp.delete(timestamp);
	}
}
