import type { Group, Result, Track, TrackSource } from "../source.ts";

/**
 * How long an old rendition's subscription is kept, at most, after a step up,
 * for the group it was sending to finish (ms).
 */
const RETIRE_TIMEOUT_MS = 5_000;

/** How the old rendition's subscription is ended when switching. */
export type Handover =
	/**
	 * Kept until it has sent every group before the new rendition's first, so
	 * playback runs on without a gap: for a step up, when the link is fine.
	 */
	| "seamless"
	/**
	 * Ended as soon as the new rendition delivers a group, freeing the link
	 * at once: for a step down, when the link is what is short.
	 */
	| "immediate";

// One subscription to one rendition.
interface Sub {
	readonly rendition: string;
	readonly track: Track;
	// Set once the subscription has delivered its first group.
	live: boolean;
	// Superseded by a newer subscription: delivers only groups before cutoff.
	retired: boolean;
	closed: boolean;
	// Ends a retired subscription that never sends a group at the cutoff.
	timer?: number;
}

/**
 * One video track made of several renditions' tracks, switching between them
 * at group boundaries.
 *
 * Renditions number their groups alike, group N of each starting at the same
 * frame, so the groups of one can follow the groups of another as though they
 * were one track. A switch subscribes to the new rendition while the old one
 * plays on. The new one's first group, numbered past every group handed out
 * so far, becomes the cutoff: from it on groups come from the new rendition,
 * and before it from the old, which is then let go (see {@link Handover}).
 *
 * {@link renditionOf} says which rendition each group handed out came from,
 * so the decoder can be configured for it when its first frame is decoded.
 */
export class SwitchingTrack implements Track {
	readonly #source: TrackSource;
	readonly #broadcast: string;
	readonly #done: Promise<void>;

	#subs: Sub[] = [];
	// The rendition being switched to, or played.
	#target: string;
	// The first group of the newest rendition, once it has delivered one.
	#cutoff: number | undefined;
	// The subscription being switched to, until it plays.
	#next: Sub | undefined;
	// The group the switch waits for, when the new rendition joined at one
	// already handed out.
	#reserved: number | undefined;
	// The highest group sequence handed out.
	#highest = -1;
	readonly #handedOut = new Map<number, string>();
	#handover: Handover = "seamless";

	readonly #queue: Group[] = [];
	readonly #watched = new WeakSet<Promise<void>>();
	readonly #signalled = new WeakSet<Promise<void>>();
	#wake: (() => void) | undefined;
	#ended: Error | undefined;
	#closed = false;

	/** Called when playback has moved to a rendition: its first group was handed out. */
	onswitch: ((rendition: string) => void) | undefined;

	private constructor(
		source: TrackSource,
		broadcast: string,
		rendition: string,
		done: Promise<void>,
	) {
		this.#source = source;
		this.#broadcast = broadcast;
		this.#target = rendition;
		this.#done = done;
	}

	/** Subscribes to `rendition` of `broadcast`, the track's first. */
	static async open(
		source: TrackSource,
		broadcast: string,
		rendition: string,
		done: Promise<void>,
	): Promise<Result<SwitchingTrack>> {
		const st = new SwitchingTrack(source, broadcast, rendition, done);
		const [track, err] = await source.subscribe(broadcast, rendition);
		if (track === undefined) return [undefined, err];
		st.#start({ rendition, track, live: false, retired: false, closed: false });
		return [st, undefined];
	}

	/** The rendition being played, or being switched to. */
	get rendition(): string {
		return this.#target;
	}

	/** The rendition being played: the one whose groups are handed out now. */
	get playing(): string | undefined {
		return this.#playing();
	}

	/** The rendition group `sequence` came from, if it was handed out. */
	renditionOf(sequence: number): string | undefined {
		return this.#handedOut.get(sequence);
	}

	/**
	 * Switches to `rendition`. A switch still under way, not yet delivering, is
	 * abandoned for this one.
	 */
	async switchTo(rendition: string, handover: Handover): Promise<Error | undefined> {
		if (this.#closed || rendition === this.#target) return undefined;
		// A switch still waiting to play is given up, and with it any group
		// reserved for it.
		if (this.#next !== undefined) this.#close(this.#next);
		this.#next = undefined;
		this.#reserved = undefined;
		this.#target = rendition;
		this.#handover = handover;
		const [track, err] = await this.#source.subscribe(this.#broadcast, rendition);
		if (track === undefined) {
			if (this.#target === rendition) {
				// Stay with what is playing.
				this.#target = this.#playing() ?? this.#target;
			}
			return err;
		}
		if (this.#closed || this.#target !== rendition) {
			track.close(); // switched again, or closed, meanwhile
			return undefined;
		}
		this.#start({ rendition, track, live: false, retired: false, closed: false });
		return undefined;
	}

	async acceptGroup(signal: Promise<void>): Promise<Result<Group>> {
		this.#watch(signal);
		for (;;) {
			const group = this.#queue.shift();
			if (group !== undefined) return [group, undefined];
			if (this.#ended !== undefined) return [undefined, this.#ended];
			if (this.#signalled.has(signal)) return [undefined, new Error("stopped")];
			await new Promise<void>((resolve) => this.#wake = resolve);
			this.#wake = undefined;
		}
	}

	// Wakes a waiting acceptGroup when `signal` resolves. Each signal is
	// watched once: the reader passes the same long-lived one to every call,
	// and a handler added per call would pile up on it until playback stops.
	#watch(signal: Promise<void>): void {
		if (this.#watched.has(signal)) return;
		this.#watched.add(signal);
		void signal.then(() => {
			this.#signalled.add(signal);
			this.#wake?.();
		});
	}

	close(): void {
		this.#closed = true;
		for (const sub of this.#subs) this.#close(sub);
		for (const group of this.#queue.splice(0)) group.cancel();
		this.#end(new Error("closed"));
	}

	// The rendition of the live subscription not retired, if any.
	#playing(): string | undefined {
		return this.#subs.find((s) => s.live && !s.retired)?.rendition;
	}

	#start(sub: Sub): void {
		this.#next = sub;
		this.#subs.push(sub);
		void this.#pump(sub);
	}

	async #pump(sub: Sub): Promise<void> {
		for (;;) {
			const [group, err] = await sub.track.acceptGroup(this.#done);
			if (group === undefined) {
				const current = !sub.retired && !sub.closed && sub.rendition === this.#target;
				this.#close(sub);
				// Only the rendition being played ending ends the track.
				if (current) this.#end(err);
				return;
			}
			this.#accept(sub, group);
		}
	}

	#accept(sub: Sub, group: Group): void {
		const seq = group.sequence;
		if (sub.closed) {
			group.cancel();
			return;
		}
		if (sub === this.#next) {
			this.#acceptNext(sub, group);
			return;
		}
		// A retired rendition sends up to the cutoff; the one playing, while a
		// switch waits for a reserved group, up to that.
		const limit = sub.retired ? this.#cutoff : this.#reserved;
		if (limit !== undefined && seq >= limit) {
			// Everything before the limit has been sent.
			group.cancel();
			this.#close(sub);
			return;
		}
		if (this.#handedOut.has(seq)) {
			group.cancel();
			return;
		}
		this.#hand(sub, group);
	}

	// A group of the rendition being switched to, before it plays.
	#acceptNext(sub: Sub, group: Group): void {
		const seq = group.sequence;
		if (this.#reserved === undefined && seq <= this.#highest) {
			// It joined at a group already handed out. The next group would
			// reach both renditions at once, and could go either way: it is
			// reserved for this one, and the one playing stops before it.
			this.#reserved = this.#highest + 1;
			group.cancel();
			return;
		}
		if (this.#reserved !== undefined && seq < this.#reserved) {
			group.cancel();
			return;
		}
		// The new rendition plays from here.
		this.#next = undefined;
		this.#reserved = undefined;
		sub.live = true;
		this.#cutoff = seq;
		for (const old of this.#subs) {
			if (old === sub || old.closed) continue;
			old.retired = true;
			if (this.#handover === "immediate") {
				this.#close(old);
			} else {
				old.timer = setTimeout(() => this.#close(old), RETIRE_TIMEOUT_MS);
			}
		}
		this.onswitch?.(sub.rendition);
		this.#hand(sub, group);
	}

	#hand(sub: Sub, group: Group): void {
		this.#handedOut.set(group.sequence, sub.rendition);
		// Only recent groups are asked about.
		if (this.#handedOut.size > 64) {
			const oldest = this.#handedOut.keys().next().value;
			if (oldest !== undefined) this.#handedOut.delete(oldest);
		}
		this.#highest = Math.max(this.#highest, group.sequence);
		this.#queue.push(group);
		this.#wake?.();
	}

	#close(sub: Sub): void {
		if (sub.closed) return;
		sub.closed = true;
		clearTimeout(sub.timer);
		sub.track.close();
		this.#subs = this.#subs.filter((s) => s !== sub);
	}

	#end(err: Error): void {
		this.#ended ??= err;
		this.#wake?.();
	}
}
