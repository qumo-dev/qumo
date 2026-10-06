// Which stretch of time the timeline shows: how much (the zoom) and, when it
// has been moved off the present, where (the pan). Free of the browser, so the
// rules can be tested.

/** What the timeline is showing. */
export interface View {
	/** How much time is in view, in milliseconds. */
	readonly span: number;
	/**
	 * Where the view ends, on the recorder's clock, once it has been moved
	 * off the present. Undefined while it is live: ending now, and following.
	 */
	readonly end: number | undefined;
}

/** A stretch of time on the recorder's clock. */
export interface Window {
	readonly from: number;
	readonly to: number;
}

/** The least and the most the timeline can show, in milliseconds. */
export const MIN_SPAN_MS = 500;
export const MAX_SPAN_MS = 60_000;

/** The view the timeline opens with: everything kept, up to now. */
export const LIVE: View = { span: MAX_SPAN_MS, end: undefined };

function clampSpan(span: number): number {
	return Math.min(MAX_SPAN_MS, Math.max(MIN_SPAN_MS, span));
}

// A view of `span` ending at `end`, kept inside what is recorded: nothing
// after now, nothing from before the oldest that is kept. One that reaches
// now is live again.
function settle(span: number, end: number, now: number): View {
	const width = clampSpan(span);
	const earliest = now - MAX_SPAN_MS + width;
	const to = Math.max(earliest, end);
	return { span: width, end: to >= now ? undefined : to };
}

/** The stretch of time `view` shows at `now`. */
export function windowOf(view: View, now: number): Window {
	const { span, end } = settle(view.span, view.end ?? now, now);
	const to = end ?? now;
	return { from: to - span, to };
}

/** Whether the view ends at the present and follows it. */
export function isLive(view: View, now: number): boolean {
	return settle(view.span, view.end ?? now, now).end === undefined;
}

/**
 * `view` with `factor` times as much time in it: under 1 zooms in, over 1
 * zooms out.
 *
 * A live view stays live, growing or shrinking from the present. One that
 * has been moved keeps the moment at `anchor` where it is, so that zooming
 * with the pointer on something keeps it under the pointer.
 *
 * @param anchor - How far along the view the fixed point is, from 0 (its
 *   start) to 1 (its end).
 */
export function zoomed(view: View, now: number, factor: number, anchor: number): View {
	const span = clampSpan(view.span * factor);
	if (view.end === undefined) return { span, end: undefined };

	const { from } = windowOf(view, now);
	const held = from + anchor * view.span;
	return settle(span, held + (1 - anchor) * span, now);
}

/**
 * `view` moved by `ms` milliseconds: back in time when negative, towards the
 * present when positive. Moving it up to the present makes it live again.
 */
export function panned(view: View, now: number, ms: number): View {
	const { to } = windowOf(view, now);
	return settle(view.span, to + ms, now);
}

// How much one notch of a mouse wheel changes the span, and how far a wheel
// event reports having turned for one notch. A trackpad reports many small
// turns, a fast flick one large one; both come out in proportion.
const NOTCH_FACTOR = 1.2;
const NOTCH_PIXELS = 100;
// Lines and pages, the other units a wheel event may count in, as pixels.
const LINE_PIXELS = 33;
const PAGE_PIXELS = 800;
// The most one event may zoom by, in notches.
const MAX_NOTCHES = 4;

/**
 * The zoom factor for a wheel turned by `delta`: over 1 (out) when turned
 * towards the user, under 1 (in) when turned away.
 *
 * @param delta - How far it turned, as `WheelEvent.deltaY` gives it.
 * @param mode - The unit of `delta`, as `WheelEvent.deltaMode` gives it:
 *   0 for pixels, 1 for lines, 2 for pages.
 */
export function wheelFactor(delta: number, mode: number): number {
	const pixels = delta * (mode === 1 ? LINE_PIXELS : mode === 2 ? PAGE_PIXELS : 1);
	const notches = Math.min(MAX_NOTCHES, Math.max(-MAX_NOTCHES, pixels / NOTCH_PIXELS));
	return NOTCH_FACTOR ** notches;
}

/** `view` showing `span` milliseconds, ending where it ended. */
export function spanned(view: View, now: number, span: number): View {
	if (view.end === undefined) return { span: clampSpan(span), end: undefined };
	return settle(span, view.end, now);
}
