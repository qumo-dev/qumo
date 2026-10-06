// A timer that keeps time in a background tab.
//
// A page's own timers are slowed to one a second or less while its tab is in
// the background. A worker's are not, so the ticks come from one.

/**
 * Calls `tick` every `intervalMs` milliseconds, from a worker's timer.
 *
 * `tick` is given the moment the worker sent the tick, in milliseconds, as
 * `performance.timeOrigin + performance.now()` read in the worker. The page
 * reads the same sum, but each thread works out its own origin, so the two
 * may differ by a fixed amount: a caller that compares them has to allow
 * for that.
 *
 * @param failed - Called if the worker fails after it was made. A browser
 *   that refuses a worker, such as under a content security policy that does
 *   not allow one made from a blob, may throw, or may only report it later.
 * @returns A function that ends the ticking.
 * @throws If the browser refuses the worker outright.
 */
export function workerTicker(
	intervalMs: number,
	tick: (sentAt: number) => void,
	failed?: (err: unknown) => void,
): () => void {
	const source =
		`setInterval(() => postMessage(performance.timeOrigin + performance.now()), ${intervalMs});`;
	const url = URL.createObjectURL(new Blob([source], { type: "text/javascript" }));
	let worker: Worker;
	try {
		worker = new Worker(url);
	} catch (err) {
		URL.revokeObjectURL(url);
		throw err;
	}
	worker.onerror = (event) => failed?.(event.error ?? event.message);
	worker.onmessage = ({ data }: MessageEvent<unknown>) => {
		// The worker is this module's own, but a message is untyped all the same.
		if (typeof data === "number") tick(data);
	};
	return () => {
		worker.terminate();
		URL.revokeObjectURL(url);
	};
}
