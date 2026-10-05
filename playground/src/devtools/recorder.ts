// Keeps a record of what happened to each track's groups, for the DevTools
// panel. It is fed by explicit calls and knows nothing of the player or of
// SolidJS: its method set happens to match the player's PlaybackObserver.
//
// A publisher records the tracks it sends through the same calls: a group
// "arrives" when it is opened and a frame "arrives" when it is written.

import { EventLog, type LogEntry } from "./log.ts";

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

/** The audio jitter buffer's state, in milliseconds unless noted. */
export interface AudioBufferRecord {
	readonly buffered: number;
	/** The least that was buffered since the previous report. */
	readonly low: number;
	/** How much is held before playing, and refilled to after running dry. */
	readonly latency: number;
	readonly stalled: boolean;
	/** Times playback ran dry (a count). */
	readonly underruns: number;
	/** Silence played because playback was held back or had run dry. */
	readonly starved: number;
	/** Silence played where audio was missing. */
	readonly gaps: number;
	/** Audio that arrived after its time had been played. */
	readonly late: number;
	/** Audio given up because more arrived than the buffer holds. */
	readonly overflowed: number;
	/** Audio skipped because the buffer was holding more than it ever used. */
	readonly trimmed: number;
	/** How far the media has been written to, on its own timeline. */
	readonly written: number;
	/** How much the output has played, audio or silence. */
	readonly played: number;
}

/** The playback delay and what it is sized from, in milliseconds. */
export interface PlaybackTimingRecord {
	/** How far playback trails the live edge. */
	readonly delay: number;
	/** How late audio has recently arrived against its fastest arrival. */
	readonly audioJitter: number;
	/** The same for video. */
	readonly videoJitter: number;
}

/** A stretch during which a track's media was not moving. */
export interface DelayRecord {
	readonly track: string;
	/**
	 * "arrival": nothing of the track came off the transport.
	 * "held": frames that had arrived waited in the player for an earlier group.
	 */
	readonly kind: "arrival" | "held";
	/** When it ended, in milliseconds on the recorder's clock. */
	readonly at: number;
	/** How long it lasted, in milliseconds. */
	readonly duration: number;
}

/** A stretch during which the page's main thread did nothing else. */
export interface StallRecord {
	/** When it ended, in milliseconds on the recorder's clock. */
	readonly at: number;
	/** How long it lasted, in milliseconds. */
	readonly duration: number;
}

/** The audio jitter buffer's state at one moment. */
export interface AudioBufferSample extends AudioBufferRecord {
	/** When it was reported, in milliseconds on the recorder's clock. */
	readonly at: number;
}

const WINDOW_MS = 60_000;
// A track with nothing arriving for this long has a gap worth showing. Audio
// from RTMP and RTSP arrives about 80 ms at a time, which is not a gap.
const ARRIVAL_GAP_MS = 120;
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
	// When the track's latest frame arrived.
	arrivedAt: number | undefined;
}

// A rise in one of the audio buffer's counters smaller than this, between two
// reports, is rounding.
const LOSS_MS = 0.5;
const DELAY_STEP_MS = 5;
const AUDIO_LOSSES = ["starved", "gaps", "late", "overflowed", "trimmed"] as const;

export class Recorder {
	readonly #now: () => number;
	readonly #log = new EventLog();
	// What the log last saw, to tell a change from a repeat.
	#loggedAudio: AudioBufferRecord | undefined;
	#loggedDelay: number | undefined;
	readonly #tracks = new Map<string, Track>();
	// The jitter buffer's reports within the retention window, oldest first.
	#audioBuffer: AudioBufferSample[] = [];
	#timing: PlaybackTimingRecord | undefined;
	#stalls: StallRecord[] = [];
	#delays: DelayRecord[] = [];

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

		const now = this.#now();
		if (t.arrivedAt !== undefined && now - t.arrivedAt >= ARRIVAL_GAP_MS) {
			this.#delayed({ track, kind: "arrival", at: now, duration: now - t.arrivedAt });
		}
		t.arrivedAt = now;

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
		if (outcome === "skipped" || outcome === "aborted" || outcome === "late") {
			this.#log.add(this.#now(), {
				kind: "group",
				track,
				problem: outcome,
				first: group,
				last: group,
				count: 1,
			});
		}

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

	/**
	 * A new stretch of playback is starting. The audio buffer's history and
	 * the time since each track's last frame belong to the one before: carried
	 * over, the time spent stopped would read as media that did not arrive,
	 * and the buffer's clocks would be measured across the stop.
	 */
	playbackStarted(): void {
		this.#log.add(this.#now(), { kind: "started" });
		this.#loggedDelay = undefined;
		this.#audioBuffer.length = 0;
		for (const track of this.#tracks.values()) {
			if (track.renders) track.arrivedAt = undefined;
		}
	}

	audioBuffer(stats: AudioBufferRecord): void {
		const at = this.#now();
		this.#logAudio(at, stats);
		const history = this.#audioBuffer;
		history.push({ ...stats, at });
		// Reports come several times a second, so prune as they arrive.
		const cutoff = at - WINDOW_MS;
		let stale = 0;
		while ((history[stale]?.at ?? Infinity) < cutoff) stale++;
		if (stale > 0) history.splice(0, stale);
	}

	/** Records that a frame of `track` was handed on after waiting `held` milliseconds in the player. */
	frameHeld(track: string, held: number): void {
		this.#delayed({ track, kind: "held", at: this.#now(), duration: held });
	}

	/** The recent stretches in which a track's media was not moving, oldest first. */
	delays(): DelayRecord[] {
		return this.#delays.slice();
	}

	// The counters only grow while one output plays, so each rise since the
	// last report is sound that was lost or given up in between.
	#logAudio(at: number, stats: AudioBufferRecord): void {
		const before = this.#loggedAudio;
		this.#loggedAudio = stats;
		if (before === undefined) return;

		if (stats.underruns > before.underruns) {
			this.#log.add(at, { kind: "ranDry", count: stats.underruns - before.underruns });
		}
		for (const loss of AUDIO_LOSSES) {
			const ms = stats[loss] - before[loss];
			if (ms >= LOSS_MS) this.#log.add(at, { kind: "audio", loss, ms });
		}
	}

	/** What has happened, in words, oldest first. */
	log(): LogEntry[] {
		return this.#log.entries();
	}

	#delayed(delay: DelayRecord): void {
		// A frame held for an earlier group is the player working as meant;
		// nothing arriving at all is worth a line.
		if (delay.kind === "arrival") {
			this.#log.add(delay.at, {
				kind: "arrival",
				track: delay.track,
				ms: delay.duration,
				count: 1,
			});
		}
		this.#delays.push(delay);
		const cutoff = delay.at - WINDOW_MS;
		let stale = 0;
		while ((this.#delays[stale]?.at ?? Infinity) < cutoff) stale++;
		if (stale > 0) this.#delays.splice(0, stale);
	}

	/** Records that the main thread has just come back from a stop of `duration` milliseconds. */
	mainThreadStalled(duration: number): void {
		const at = this.#now();
		this.#log.add(at, { kind: "stall", ms: duration, count: 1 });
		this.#stalls.push({ at, duration });
		const cutoff = at - WINDOW_MS;
		let stale = 0;
		while ((this.#stalls[stale]?.at ?? Infinity) < cutoff) stale++;
		if (stale > 0) this.#stalls.splice(0, stale);
	}

	/** The main thread's recent stops, oldest first. */
	stalls(): StallRecord[] {
		return this.#stalls.slice();
	}

	playbackTiming(timing: PlaybackTimingRecord): void {
		// The delay creeps up by fractions as the jitter estimate does; only a
		// step someone could notice is worth a line.
		const logged = this.#loggedDelay;
		if (logged === undefined || Math.abs(timing.delay - logged) >= DELAY_STEP_MS) {
			this.#log.add(this.#now(), {
				kind: "delay",
				from: this.#loggedDelay,
				to: timing.delay,
			});
			this.#loggedDelay = timing.delay;
		}
		this.#timing = timing;
	}

	/** The latest playback delay and arrival jitter; undefined until playback has begun. */
	timing(): PlaybackTimingRecord | undefined {
		return this.#timing;
	}

	/**
	 * The audio jitter buffer's reports within the retention window, oldest
	 * first. Empty until audio has played.
	 */
	audio(): AudioBufferSample[] {
		return this.#audioBuffer.slice();
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
				arrivedAt: undefined,
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
