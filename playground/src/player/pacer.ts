/**
 * Holds items back for a time, releasing them in the order they were pushed.
 *
 * Order wins over timing: when an item's time comes, every item pushed before
 * it is released first, even if its own time has not. So a later item asked to
 * wait less never overtakes an earlier one.
 */
export class Pacer<T> {
	readonly #emit: (item: T) => void;
	readonly #drop: (item: T) => void;
	readonly #pending: { item: T; timer: number }[] = [];

	/**
	 * @param emit - Receives each item when it is released.
	 * @param drop - Receives each item discarded by {@link clear}.
	 */
	constructor(emit: (item: T) => void, drop: (item: T) => void) {
		this.#emit = emit;
		this.#drop = drop;
	}

	/** Releases `item` after `delay` milliseconds; at once if `delay` is not positive. */
	push(item: T, delay: number): void {
		if (!(delay > 0)) {
			this.#releaseAll();
			this.#emit(item);
			return;
		}

		const entry = { item, timer: 0 };
		entry.timer = setTimeout(() => this.#releaseThrough(entry), delay);
		this.#pending.push(entry);
	}

	/** Discards everything still held. */
	clear(): void {
		for (const entry of this.#pending.splice(0)) {
			clearTimeout(entry.timer);
			this.#drop(entry.item);
		}
	}

	#releaseAll(): void {
		for (const entry of this.#pending.splice(0)) {
			clearTimeout(entry.timer);
			this.#emit(entry.item);
		}
	}

	#releaseThrough(last: { item: T; timer: number }): void {
		const count = this.#pending.indexOf(last) + 1;
		for (const entry of this.#pending.splice(0, count)) {
			clearTimeout(entry.timer);
			this.#emit(entry.item);
		}
	}
}
