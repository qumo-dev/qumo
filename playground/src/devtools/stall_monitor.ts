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

	/**
	 * Takes one tick. Both times are milliseconds on the same clock.
	 *
	 * @param sentAt - When the tick was sent.
	 * @param now - When it is being handled.
	 * @returns How long the main thread had stopped, if this tick shows that
	 *   it had; undefined otherwise.
	 */
	handled(sentAt: number, now: number): number | undefined {
		const sinceLast = this.#handledAt === undefined ? Infinity : now - this.#handledAt;
		this.#handledAt = now;

		const waited = now - sentAt;
		if (waited < MIN_STALL_MS) return undefined;
		// The ticks that queued up behind a stop are handled together once it
		// ends, each having waited a little less. The first is the stop; the
		// rest, handled right after it, are the same one.
		if (sinceLast < MIN_STALL_MS) return undefined;
		return waited;
	}
}

/**
 * Calls `report` with the length, in milliseconds, of each stop of the main
 * thread, whether or not the tab is in view. Returns a function that ends
 * the watch.
 */
export function watchMainThread(report: (duration: number) => void): () => void {
	const detector = new StallDetector();
	try {
		return workerTicker(TICK_MS, (sentAt) => {
			const stopped = detector.handled(sentAt, performance.timeOrigin + performance.now());
			if (stopped !== undefined) report(stopped);
		});
	} catch (err) {
		// Without the ticker there is nothing to measure against; the panel
		// then shows no stops, and says nothing false.
		log.warn("the page's stops cannot be watched", { err });
		return () => {};
	}
}
