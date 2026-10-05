// The audio jitter buffer: a ring of PCM indexed by media time.
//
// Every block is written at the sample position its timestamp names, so
// blocks may arrive out of order or in bursts and still play in place, and a
// block that never arrives leaves silence of exactly its own length. Playback
// holds back until `latency` of audio is buffered, and holds back again
// whenever it runs dry, instead of stuttering along an empty buffer.
//
// The design follows the moq-dev reference player's AudioRingBuffer
// (js/watch/src/audio/ring-buffer.ts, MIT / Apache-2.0).

export interface AudioRingInit {
	/** Samples per second, the rate playback runs at. */
	rate: number;
	/** Channels played. A block with fewer has its first channel repeated. */
	channels: number;
	/** Milliseconds of audio to hold before playing, and to refill to after running dry. */
	latency: number;
	/**
	 * Milliseconds of room above the latency, for audio that arrives in
	 * bursts. Defaults to the latency or 200 ms, whichever is more.
	 */
	headroom?: number;
}

/** What the buffer has had to do so far, in samples unless noted. */
export interface AudioRingStats {
	/** Buffered and not yet played. */
	buffered: number;
	/** How much is held before playing, and refilled to after running dry. */
	latency: number;
	/** True while playback is held back, waiting to (re)fill. */
	stalled: boolean;
	/** Times playback ran dry (a count). */
	underruns: number;
	/** Silence played because playback was held back or had run dry. */
	starved: number;
	/** Silence written where a block was missing. */
	gaps: number;
	/** Arrived after their time had been played, and dropped. */
	late: number;
	/** Given up because more arrived than the buffer holds. */
	overflowed: number;
	/** Skipped because the buffer was holding more than it ever used. */
	trimmed: number;
	/**
	 * How far the media has been written to, as a position on its timeline.
	 * Its rate of change is the speed of the source's clock.
	 */
	written: number;
	/**
	 * Samples the output has asked for, audio or silence. Its rate of change
	 * is the speed of the output's clock.
	 */
	played: number;
}

export class AudioRing {
	readonly rate: number;
	readonly channels: number;

	#buffer: Float32Array[];
	// Sample positions on the media timeline. Everything in [read, write) is
	// buffered; the ring slot of position p is p % capacity.
	#read = 0;
	#write = 0;
	// Whether the positions have been set from a first block.
	#anchored = false;
	#stalled = true;
	#latency: number; // samples
	readonly #headroom: number | undefined; // samples
	#underruns = 0;
	#starved = 0;
	#gaps = 0;
	#late = 0;
	#overflowed = 0;
	#trimmed = 0;
	#played = 0;
	// The lowest the buffer has been, and the samples played, since the last
	// check for surplus.
	#floor = Infinity;
	#floorSpan = 0;
	// The lowest the buffer has been since takeLow was last called.
	#low = Infinity;
	// How far, in samples, a block may sit from the end of the last one and
	// still be joined to it: one millisecond.
	readonly #snap: number;

	constructor(init: AudioRingInit) {
		if (!(init.rate > 0)) throw new RangeError("rate must be positive");
		if (!(init.channels > 0)) throw new RangeError("channels must be positive");

		this.rate = init.rate;
		this.channels = init.channels;
		this.#snap = Math.ceil(init.rate / 1000);
		this.#latency = this.#samples(init.latency);
		this.#headroom = init.headroom === undefined ? undefined : this.#samples(init.headroom);
		this.#buffer = allocate(this.channels, this.#capacityFor(this.#latency));
	}

	/** True while playback is held back, waiting for `latency` of audio. */
	get stalled(): boolean {
		return this.#stalled;
	}

	/** Samples buffered and not yet played. */
	get buffered(): number {
		return this.#write - this.#read;
	}

	/**
	 * The least that has been buffered since this was last asked, and starts
	 * the next stretch. The level a moment ago says little: audio arrives in
	 * bursts, so the buffer swings, and it is the bottom of the swing that
	 * shows how close playback came to running dry.
	 */
	takeLow(): number {
		const low = Math.min(this.#low, this.buffered);
		this.#low = Infinity;
		return low;
	}

	/** How many times playback has run dry. */
	get underruns(): number {
		return this.#underruns;
	}

	get stats(): AudioRingStats {
		return {
			buffered: this.buffered,
			latency: this.#latency,
			stalled: this.#stalled,
			underruns: this.#underruns,
			starved: this.#starved,
			gaps: this.#gaps,
			late: this.#late,
			overflowed: this.#overflowed,
			trimmed: this.#trimmed,
			written: this.#write,
			played: this.#played,
		};
	}

	/**
	 * Changes how much audio is held before playing. Raising it holds
	 * playback back until the buffer covers the new amount; what is already
	 * buffered is kept either way.
	 */
	resize(latency: number): void {
		const samples = this.#samples(latency);
		if (samples > this.#latency) this.#stalled = true;
		this.#latency = samples;

		const capacity = this.#capacityFor(samples);
		if (capacity !== this.#capacity) this.#reallocate(capacity);
		if (this.buffered >= this.#latency) this.#stalled = false;
	}

	/**
	 * Forgets what is buffered and where the timeline was, ready for a new
	 * stream. The counts of what it has had to do are kept.
	 */
	reset(): void {
		this.#read = 0;
		this.#write = 0;
		this.#anchored = false;
		this.#stalled = true;
		this.#floor = Infinity;
		this.#floorSpan = 0;
	}

	/**
	 * Buffers a block whose first sample has media time `timestamp`
	 * (microseconds). Samples whose time has already been played are dropped.
	 */
	write(timestamp: number, block: readonly Float32Array[]): void {
		const first = block[0];
		if (first === undefined || first.length === 0) return;
		const frames = first.length;
		const capacity = this.#capacity;

		let start = Math.round(timestamp * this.rate / 1_000_000);

		// A block a whole buffer behind playback is not late audio: the
		// timeline has restarted (a new publisher, a reset clock). Follow it.
		if (this.#anchored && this.#read - (start + frames) > capacity) this.#anchored = false;
		if (!this.#anchored) {
			this.#read = start;
			this.#write = start;
			this.#anchored = true;
			this.#stalled = true;
		}

		// Timestamps are rounded, to microseconds or (from RTMP) to whole
		// milliseconds, so a block that follows the last one exactly can land
		// a little early or late. Joining it up avoids a hole or an overlap
		// of a few samples at every block, which is audible as a click. A
		// block that is really missing is far longer than this.
		if (Math.abs(start - this.#write) <= this.#snap) start = this.#write;

		const end = start + frames;
		if (end <= this.#read) {
			this.#late += frames;
			return;
		}

		// More than the ring can hold: playback has fallen behind the media
		// (a burst after a slow spell, or audio arriving while output was not
		// running). Catch up to the cushion in one step, giving up the oldest
		// audio. Trimming only what does not fit would leave the ring full,
		// and every burst after that would spill a little more: a click each.
		let overflowing = false;
		if (end - this.#read > capacity) {
			const target = end - this.#latency;
			this.#overflowed += Math.max(0, Math.min(this.#write, target) - this.#read);
			this.#read = target;
			this.#stalled = false;
			overflowing = true;
		}

		// Whatever of this block is behind playback is not played.
		const skip = Math.max(0, this.#read - start);
		if (overflowing) this.#overflowed += skip;
		else this.#late += skip;
		start += skip;

		// Silence where nothing was written between the last block and this one.
		const gapFrom = Math.max(this.#write, this.#read);
		if (start > gapFrom) this.#gaps += start - gapFrom;
		for (let position = gapFrom; position < start; position++) {
			for (const channel of this.#buffer) channel[position % capacity] = 0;
		}

		for (let c = 0; c < this.channels; c++) {
			const source = block[c] ?? first;
			const channel = this.#buffer[c];
			if (channel === undefined) continue;
			for (let i = 0; i < end - start; i++) {
				channel[(start + i) % capacity] = source[skip + i] ?? 0;
			}
		}

		if (end > this.#write) this.#write = end;
		if (this.buffered >= this.#latency) this.#stalled = false;
	}

	/**
	 * Fills `output` with the next samples and returns how many were audio;
	 * the rest is silence. Running dry holds playback back until the buffer
	 * has refilled.
	 */
	read(output: readonly Float32Array[]): number {
		const want = output[0]?.length ?? 0;
		const samples = this.#stalled ? 0 : Math.min(this.buffered, want);
		const capacity = this.#capacity;

		for (let c = 0; c < output.length; c++) {
			const destination = output[c];
			const channel = this.#buffer[Math.min(c, this.channels - 1)];
			if (destination === undefined) continue;
			if (channel !== undefined) {
				for (let i = 0; i < samples; i++) {
					destination[i] = channel[(this.#read + i) % capacity] ?? 0;
				}
			}
			destination.fill(0, samples);
		}

		this.#read += samples;
		this.#played += want;
		this.#low = Math.min(this.#low, this.buffered);
		// Silence before the first block ever arrives is not starvation.
		if (this.#anchored) this.#starved += want - samples;
		if (!this.#stalled && samples < want) {
			this.#stalled = true;
			this.#underruns++;
		}
		this.#trimSurplus(want);
		return samples;
	}

	// Playback resumes whenever the latency is buffered, but nothing brings
	// the level back down if it ends up higher: after a burst, or when audio
	// arrived for a while with nothing playing it. Audio would then run that
	// much later than intended, and later than the video. So watch how low the
	// buffer gets: if over a stretch of playback it never came down to the
	// latency, the part above it was never needed, and is skipped.
	#trimSurplus(played: number): void {
		if (this.#stalled) {
			this.#floor = Infinity;
			this.#floorSpan = 0;
			return;
		}

		this.#floor = Math.min(this.#floor, this.buffered);
		this.#floorSpan += played;
		if (this.#floorSpan < this.rate * SURPLUS_WINDOW_S) return;

		// A few milliseconds over is not worth a skip, which is itself audible.
		const surplus = this.#floor - this.#latency;
		if (surplus > this.#snap * MIN_SURPLUS_MS) {
			this.#read += surplus;
			this.#trimmed += surplus;
		}
		this.#floor = Infinity;
		this.#floorSpan = 0;
	}

	get #capacity(): number {
		return this.#buffer[0]?.length ?? 0;
	}

	#samples(milliseconds: number): number {
		if (!(milliseconds > 0)) throw new RangeError("latency must be positive");
		return Math.ceil(this.rate * milliseconds / 1000);
	}

	// Room for the latency itself plus headroom. Playback resumes as soon as
	// the latency is buffered, which can be at the top of a burst, so the
	// level then swings between about the latency and the latency plus a
	// burst. Sources that send several frames at once (RTMP and RTSP audio
	// comes about 80 ms at a time) need that swing to fit, or every large
	// burst would overflow and force a catch-up.
	#capacityFor(latency: number): number {
		return latency + (this.#headroom ?? Math.max(latency, this.#samples(DEFAULT_HEADROOM_MS)));
	}

	// Moves to a ring of `capacity`, keeping the newest audio that fits.
	#reallocate(capacity: number): void {
		const next = allocate(this.channels, capacity);
		const keep = Math.min(this.buffered, capacity);
		const from = this.#write - keep;
		const old = this.#capacity;

		for (let c = 0; c < this.channels; c++) {
			const source = this.#buffer[c];
			const destination = next[c];
			if (source === undefined || destination === undefined) continue;
			for (let i = 0; i < keep; i++) {
				destination[(from + i) % capacity] = source[(from + i) % old] ?? 0;
			}
		}

		this.#buffer = next;
		this.#read = from;
	}
}

const DEFAULT_HEADROOM_MS = 200;
// How long the buffer has to stay above its latency before the surplus is skipped.
const SURPLUS_WINDOW_S = 2;
// The least surplus worth skipping, in milliseconds.
const MIN_SURPLUS_MS = 10;

function allocate(channels: number, capacity: number): Float32Array[] {
	return Array.from({ length: channels }, () => new Float32Array(capacity));
}
