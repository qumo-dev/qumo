// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertAlmostEquals, assertEquals } from "@std/assert";
import { isLive, LIVE, panned, spanned, type View, wheelFactor, windowOf, zoomed } from "./view.ts";

// The present, far enough in that a minute of history lies behind it.
const NOW = 100_000;

Deno.test("the opening view is the last minute, up to now", () => {
	const shown = windowOf(LIVE, NOW);

	assertEquals(shown, { from: 40_000, to: 100_000 });
});

Deno.test("a live view follows the present", () => {
	const view: View = { span: 10_000, end: undefined };

	const shown = [windowOf(view, NOW), windowOf(view, NOW + 5000)];

	assertEquals(shown, [{ from: 90_000, to: 100_000 }, { from: 95_000, to: 105_000 }]);
});

Deno.test("a moved view stays where it was put as time passes", () => {
	const view: View = { span: 10_000, end: 80_000 };

	const shown = [windowOf(view, NOW), windowOf(view, NOW + 5000)];

	assertEquals(shown, [{ from: 70_000, to: 80_000 }, { from: 70_000, to: 80_000 }]);
});

Deno.test("a moved view is carried along once what it showed is no longer kept", () => {
	const view: View = { span: 10_000, end: 80_000 };

	// A minute on, nothing before 100 000 is kept.
	const shown = windowOf(view, NOW + 60_000);

	assertEquals(shown, { from: 100_000, to: 110_000 });
});

Deno.test("zooming a live view keeps it live, ending now", () => {
	const cases = [
		{ name: "in", factor: 0.5, want: { span: 30_000, end: undefined } },
		{ name: "out", factor: 2, want: { span: 60_000, end: undefined } },
	] as const;

	for (const c of cases) {
		// The pointer's place is ignored: a live view grows from the present.
		assertEquals(zoomed(LIVE, NOW, c.factor, 0.25), c.want, c.name);
	}
});

Deno.test("zooming a moved view keeps the moment under the pointer in place", () => {
	// 70 000 to 80 000 in view; the pointer a quarter of the way in, on 72 500.
	const view: View = { span: 10_000, end: 80_000 };

	const closer = zoomed(view, NOW, 0.5, 0.25);

	// Half the span, with 72 500 still a quarter of the way in.
	assertEquals(windowOf(closer, NOW), { from: 71_250, to: 76_250 });
});

Deno.test("zooming stops at the least and the most that can be shown", () => {
	const cases = [
		{ name: "in", view: { span: 600, end: undefined }, factor: 0.1, want: 500 },
		{ name: "out", view: { span: 50_000, end: undefined }, factor: 4, want: 60_000 },
	] as const;

	for (const c of cases) {
		assertEquals(zoomed(c.view, NOW, c.factor, 0.5).span, c.want, c.name);
	}
});

Deno.test("zooming a moved view out until it reaches the present makes it live", () => {
	const view: View = { span: 10_000, end: 95_000 };

	const wider = zoomed(view, NOW, 6, 0);

	assertEquals(wider, { span: 60_000, end: undefined });
});

Deno.test("panning back moves the view into the past by that much", () => {
	const view: View = { span: 10_000, end: undefined };

	const back = panned(view, NOW, -15_000);

	assertEquals(windowOf(back, NOW), { from: 75_000, to: 85_000 });
});

Deno.test("panning forward to the present makes the view live again", () => {
	const view: View = { span: 10_000, end: 85_000 };

	const forward = panned(view, NOW, 20_000);

	assertEquals(forward, { span: 10_000, end: undefined });
});

Deno.test("panning stops at the oldest that is kept", () => {
	const view: View = { span: 10_000, end: undefined };

	const back = panned(view, NOW, -500_000);

	assertEquals(windowOf(back, NOW), { from: 40_000, to: 50_000 });
});

Deno.test("isLive is true only while the view ends at the present", () => {
	const cases = [
		{ name: "never moved", view: LIVE, want: true },
		{ name: "moved back", view: { span: 10_000, end: 80_000 }, want: false },
		{ name: "moved to the present", view: { span: 10_000, end: NOW }, want: true },
		// The whole minute cannot be anywhere but up to now.
		{ name: "showing everything", view: { span: 60_000, end: 70_000 }, want: true },
	] as const;

	for (const c of cases) {
		assertEquals(isLive(c.view, NOW), c.want, c.name);
	}
});

Deno.test("spanned changes how much is shown and keeps where the view ends", () => {
	const cases = [
		{
			name: "a live view",
			view: LIVE,
			want: { from: 98_000, to: 100_000 },
		},
		{
			name: "a moved view",
			view: { span: 10_000, end: 80_000 },
			want: { from: 78_000, to: 80_000 },
		},
	] as const;

	for (const c of cases) {
		assertEquals(windowOf(spanned(c.view, NOW, 2000), NOW), c.want, c.name);
	}
});

Deno.test("wheelFactor zooms in proportion to how far the wheel turned", () => {
	const cases = [
		{ name: "one notch towards the user", delta: 100, mode: 0, want: 1.2 },
		{ name: "one notch away", delta: -100, mode: 0, want: 1 / 1.2 },
		{ name: "a small trackpad turn", delta: 10, mode: 0, want: 1.2 ** 0.1 },
		{ name: "not turned", delta: 0, mode: 0, want: 1 },
		{ name: "three lines", delta: 3, mode: 1, want: 1.2 ** 0.99 },
	] as const;

	for (const c of cases) {
		assertAlmostEquals(wheelFactor(c.delta, c.mode), c.want, 1e-9, c.name);
	}
});

Deno.test("wheelFactor does not let one event zoom by more than four notches", () => {
	const cases = [
		{ name: "a flick out", delta: 5000, mode: 0, want: 1.2 ** 4 },
		{ name: "a flick in", delta: -5000, mode: 0, want: 1.2 ** -4 },
		{ name: "a page", delta: 1, mode: 2, want: 1.2 ** 4 },
	] as const;

	for (const c of cases) {
		assertAlmostEquals(wheelFactor(c.delta, c.mode), c.want, 1e-9, c.name);
	}
});
