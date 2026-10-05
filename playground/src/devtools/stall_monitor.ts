// Notices when the page's main thread stops for a while.
//
// Media reaches the decoders, and decoded audio reaches the audio thread,
// through the main thread. While it is busy with something else nothing moves,
// and a stop longer than the audio buffer is a gap in the sound. Showing the
// stops next to the audio glitches tells the two kinds of glitch apart: the
// ones the page caused, and the ones that came with the stream.

// How often the monitor expects to run. A timer due this often that comes
// late was kept waiting by something.
const TICK_MS = 20;
// A stop shorter than this is ordinary: a frame, a small collection.
const MIN_STALL_MS = 30;

/**
 * Calls `report` with the length, in milliseconds, of each stop of the main
 * thread. Returns a function that ends the watch.
 */
export function watchMainThread(report: (duration: number) => void): () => void {
	let last = performance.now();
	let hidden = document.hidden;

	const timer = setInterval(() => {
		const now = performance.now();
		const late = now - last - TICK_MS;
		last = now;
		// A hidden page has its timers slowed to once a second or less, which
		// is the browser saving work, not the page being busy. The first tick
		// after it is shown again still measures from a slowed one.
		const wasHidden = hidden;
		hidden = document.hidden;
		if (hidden || wasHidden) return;
		if (late >= MIN_STALL_MS) report(late);
	}, TICK_MS);

	return () => clearInterval(timer);
}
