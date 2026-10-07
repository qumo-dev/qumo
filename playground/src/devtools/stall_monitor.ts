// Notices when the page's main thread stops for a while.
//
// Media reaches the decoders, and decoded audio reaches the audio thread,
// through the main thread. While it is busy with something else nothing moves,
// and a stop longer than the audio buffer is a gap in the sound. Showing the
// stops next to the audio glitches tells the two kinds of glitch apart: the
// ones the page caused, and the ones that came with the stream.
import { createLogger } from "@okdaichi/media-log";
import { workerTicker } from "../worker_ticker.ts";

const log = createLogger("devtools");

// How often a tick is sent to the main thread.
const TICK_MS = 20;
// A stop shorter than this is ordinary: a frame, a small collection.
const MIN_STALL_MS = 30;
// How many ticks apart two ticks may be sent with the ticker still running.
const LATE_TICKS = 5;

/**
 * Tells, from when each tick was sent and when it was handled, whether the
 * main thread had stopped.
 *
 * The ticks come from another thread, so one that is handled long after it
 * was sent waited for the main thread, and for nothing else. That holds in a
 * background tab too, where a timer on the page itself runs late because the
 * browser slows it, not because the page is busy. And a ticker that itself
 * pauses sends nothing, which reads as no stop at all.
 */
export class StallDetector {
	// When the last tick was handled.
	#handledAt: number | undefined;
	// How far the sender's clock reads ahead of this one, at most: the least
	// any tick has seemed to wait. The two threads each work out the time
	// for themselves and need not agree; the tick that waited least waited
	// next to nothing, so what it seemed to wait is the disagreement.
	#skew = Infinity;
	readonly #tickMs: number;

	/** @param tickMs - How often the ticks are sent, in milliseconds. */
	constructor(tickMs: number) {
		this.#tickMs = tickMs;
	}

	/**
	 * Takes one tick.
	 *
	 * @param sentAt - When the tick was sent, on the sender's clock.
	 * @param now - When it is being handled, on this thread's clock.
	 * @returns How long the main thread had stopped, if this tick shows that
	 *   it had; undefined otherwise.
	 */
	handled(sentAt: number, now: number): number | undefined {
		this.#skew = Math.min(this.#skew, now - sentAt);
		// The main thread was free when it handled the last tick, so it can
		// only have been stopped since then. Counting from there is what
		// keeps the ticks that queued up behind a stop from each being taken
		// for the whole of it: the first is handled as the stop ends, and
		// the rest right after.
		const sent = sentAt + this.#skew;
		const last = this.#handledAt;
		this.#handledAt = now;

		const waited = now - Math.max(sent, last ?? -Infinity);
		if (waited < MIN_STALL_MS) return undefined;

		// The stop began some time after the last tick was handled and
		// before this one was sent. Counting from when this one was sent
		// leaves that stretch out, and reads every stop short by up to a
		// tick; the middle of it is the fairest guess. A worker's timer does
		// not keep exact time, so the stretch may be a few ticks long. After
		// a longer silence the ticker had paused, and nothing is known about
		// the time in between.
		const unseen = last === undefined ? 0 : sent - last;
		const within = unseen > 0 && unseen <= this.#tickMs * LATE_TICKS;
		return waited + (within ? unseen / 2 : 0);
	}
}

/**
 * Calls `report` with the length, in milliseconds, of each stop of the main
 * thread, whether or not the tab is in view. Returns a function that ends
 * the watch.
 */
export function watchMainThread(report: (duration: number) => void): () => void {
	const detector = new StallDetector(TICK_MS);
	try {
		return workerTicker(TICK_MS, (sentAt) => {
			const stopped = detector.handled(sentAt, performance.timeOrigin + performance.now());
			if (stopped !== undefined) report(stopped);
		}, (err) => {
			// A worker the browser refuses may say so only once it has been
			// made. The panel then shows no stops, and says nothing false.
			log.warn("the page's stops are no longer watched", { err });
		});
	} catch (err) {
		// Without the ticker there is nothing to measure against; the panel
		// then shows no stops, and says nothing false.
		log.warn("the page's stops cannot be watched", { err });
		return () => {};
	}
}
