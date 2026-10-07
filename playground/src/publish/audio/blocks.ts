/**
 * Gathers the audio thread's render quanta into blocks of a fixed length.
 *
 * The audio thread hands over 128 samples at a time, 375 times a second at
 * 48 kHz. Sending each one to the page would be 375 messages a second through
 * the thread everything else shares, so they are gathered here, on the audio
 * thread, into blocks the length of one encoded frame.
 */
export class Blocks {
	readonly #channels: number;
	readonly #length: number;
	// The block being filled: every channel's samples, one channel after another.
	#block: Float32Array<ArrayBuffer>;
	#filled = 0;

	/**
	 * @param channels - Channels in every block.
	 * @param length - Samples per channel in every block.
	 */
	constructor(channels: number, length: number) {
		this.#channels = channels;
		this.#length = length;
		this.#block = new Float32Array(channels * length);
	}

	/** Samples per channel in every block. */
	get length(): number {
		return this.#length;
	}

	/** Forgets the samples gathered towards the next block. */
	reset(): void {
		this.#filled = 0;
	}

	/**
	 * Adds one quantum. A channel the input lacks is given the first one's
	 * samples, so a mono source is heard on both sides.
	 *
	 * @param input - One array of samples per channel, all the same length.
	 * @param emit - Called with each block completed, channel after channel.
	 *   The block is the caller's to keep.
	 */
	push(
		input: readonly Float32Array[],
		emit: (block: Float32Array<ArrayBuffer>) => void,
	): void {
		const first = input[0];
		if (first === undefined) return;

		let taken = 0;
		while (taken < first.length) {
			const count = Math.min(first.length - taken, this.#length - this.#filled);
			for (let channel = 0; channel < this.#channels; channel++) {
				const samples = input[channel] ?? first;
				this.#block.set(
					samples.subarray(taken, taken + count),
					channel * this.#length + this.#filled,
				);
			}
			taken += count;
			this.#filled += count;

			if (this.#filled === this.#length) {
				emit(this.#block);
				this.#block = new Float32Array(this.#channels * this.#length);
				this.#filled = 0;
			}
		}
	}
}
