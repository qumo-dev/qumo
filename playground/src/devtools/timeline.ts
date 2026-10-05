import type {
	AudioBufferSample,
	DelayRecord,
	GroupRecord,
	GroupState,
	StallRecord,
} from "./recorder.ts";

// Turns a track's group records into what the timeline draws. A track with few
// groups shows each one as a bar; a track with many (audio sends tens a second)
// shows one column per second.

/** Above this many groups in view, a track is drawn as per-second columns. */
export const BAR_LIMIT = 120;

// A sender ends one group as it opens the next, and the two events can reach the
// reader in either order. An overlap this short is that hand-over, not a group
// arriving while another was still in flight.
const HANDOVER_MS = 50;

export interface Bar {
	readonly group: GroupRecord;
	/** Row the bar sits on, from 0. Bars sharing a row never overlap in time. */
	readonly lane: number;
	readonly start: number;
	readonly end: number;
}

/**
 * Places each group on the lowest lane where it does not overlap an earlier
 * one, so a track whose groups arrive one after another fills a single lane
 * and every extra lane is a group that overlapped. A hand-over between
 * consecutive groups does not count as overlap. A group still being received
 * runs to `now`.
 */
export function packLanes(groups: readonly GroupRecord[], now: number): Bar[] {
	const sorted = [...groups].sort((a, b) => a.arrived - b.arrived);
	const laneEnds: number[] = [];

	return sorted.map((group) => {
		const end = Math.max(group.ended ?? now, group.arrived);
		let lane = laneEnds.findIndex((laneEnd) => laneEnd <= group.arrived + HANDOVER_MS);
		if (lane === -1) lane = laneEnds.length;
		laneEnds[lane] = end;
		return { group, lane, start: group.arrived, end };
	});
}

export interface Column {
	/** Start of the second this column covers. */
	readonly start: number;
	/** Groups that arrived in it, by where they stand now. */
	readonly states: Readonly<Record<GroupState, number>>;
	readonly frames: number;
	readonly rendered: number;
}

/** Counts groups into `size`-millisecond columns covering [from, to). */
export function columns(
	groups: readonly GroupRecord[],
	from: number,
	to: number,
	size: number,
): Column[] {
	const count = Math.max(0, Math.ceil((to - from) / size));
	const out = Array.from({ length: count }, (_, i) => ({
		start: from + i * size,
		states: { receiving: 0, complete: 0, aborted: 0, skipped: 0, late: 0, stopped: 0 },
		frames: 0,
		rendered: 0,
	}));

	for (const group of groups) {
		const column = out[Math.floor((group.arrived - from) / size)];
		if (column === undefined) continue;
		column.states[group.state]++;
		column.frames += group.frames;
		column.rendered += group.rendered;
	}
	return out;
}

/** A moment at which the audio output lost or skipped sound. */
export interface AudioGlitch {
	readonly at: number;
	/** Milliseconds of sound affected between the previous report and this one, by cause. */
	readonly starved: number;
	readonly gaps: number;
	readonly late: number;
	readonly overflowed: number;
	readonly trimmed: number;
}

/** How fast two clocks ran over a stretch, as a fraction of real time. */
export interface ClockRates {
	/** The source's clock: media time that arrived per second. */
	readonly media: number;
	/** The output's clock: audio played per second. */
	readonly output: number;
}

// Rates over less than this are too noisy to show: audio arrives in bursts.
const MIN_RATE_SPAN_MS = 5000;

/**
 * Compares the clocks at the two ends of the audio buffer. They should both
 * run at 1.0. When the media clock runs faster than the output clock, audio
 * piles up until the buffer overflows; the other way round, it runs dry.
 * Undefined until there is enough history, or across an output restart.
 */
export function clockRates(history: readonly AudioBufferSample[]): ClockRates | undefined {
	const first = history[0];
	const last = history.at(-1);
	if (first === undefined || last === undefined) return undefined;

	const elapsed = last.at - first.at;
	if (elapsed < MIN_RATE_SPAN_MS || last.played < first.played) return undefined;
	return {
		media: (last.written - first.written) / elapsed,
		output: (last.played - first.played) / elapsed,
	};
}

// Less than this much lost between two reports is rounding, not a glitch.
const GLITCH_MS = 0.5;

/**
 * Finds where the audio buffer's counters rose: each rise is sound that was
 * replaced by silence or thrown away between two reports. The counters only
 * grow while one output plays; a fall means a new one started, and is not a
 * glitch.
 */
export function audioGlitches(history: readonly AudioBufferSample[]): AudioGlitch[] {
	const glitches: AudioGlitch[] = [];
	for (let i = 1; i < history.length; i++) {
		const before = history[i - 1];
		const now = history[i];
		if (before === undefined || now === undefined) continue;

		const rise = (key: "starved" | "gaps" | "late" | "overflowed" | "trimmed") =>
			Math.max(0, now[key] - before[key]);
		const glitch = {
			at: now.at,
			starved: rise("starved"),
			gaps: rise("gaps"),
			late: rise("late"),
			overflowed: rise("overflowed"),
			trimmed: rise("trimmed"),
		};
		const lost = glitch.starved + glitch.gaps + glitch.late + glitch.overflowed +
			glitch.trimmed;
		if (lost >= GLITCH_MS) glitches.push(glitch);
	}
	return glitches;
}

// ---- What the timeline draws ----
//
// The lanes are drawn on canvases. A lane is described here as plain shapes, so
// the layout can be tested without a browser and drawing is one pass over a
// list: a previous version built an SVG element per mark through the UI
// framework, and rebuilding a few hundred of them every tick held the main
// thread for 50 ms at a time, long enough to starve the audio it was showing.

/** How a shape is painted. The group states, plus the timeline's own marks. */
export type ShapeStyle =
	| GroupState
	| "rendered"
	| "unrendered"
	| "glitch"
	| "stall"
	| "arrival"
	| "held"
	| "track";

export interface Shape {
	/** Left and right edges, as fractions of the lane's width (0 = oldest, 1 = now). */
	readonly x0: number;
	readonly x1: number;
	/** Top edge and height, in pixels. */
	readonly y: number;
	readonly height: number;
	readonly style: ShapeStyle;
	/** What the pointer shows on this shape; absent for decoration. */
	readonly title?: string;
	/** The group this shape belongs to, for picking out a group's shapes together. */
	readonly group?: number;
}

export interface Lane {
	/** Height of the lane, in pixels. */
	readonly height: number;
	/** In drawing order: later shapes are on top. */
	readonly shapes: readonly Shape[];
	/** A filled line graph behind the shapes, as [x fraction, y pixels] points. */
	readonly level?: readonly (readonly [number, number])[];
}

const LANE_HEIGHT = 10;
const LANE_GAP = 2;
const MAX_LANES = 4;
const RENDER_HEIGHT = 6;
const RENDER_GAP = 5;
const COLUMN_COUNT = 60;
const COLUMN_HEIGHT = 22;
const AUDIO_HEIGHT = 24;
const DELAY_HEIGHT = 4;
// The audio level is drawn against the most it has been in view, but never
// against less than this, so a steady buffer is not stretched to look wild.
const MIN_AUDIO_CEILING_MS = 200;

// Bottom-to-top order of a stacked column: the ordinary state first, so the
// exceptions sit on top of it.
const STACK_ORDER: readonly GroupState[] = [
	"complete",
	"receiving",
	"stopped",
	"skipped",
	"aborted",
	"late",
];

/**
 * One track's lane over the `span` milliseconds up to `now`: its groups as
 * they arrived (the network view) above a strip of what was rendered. A track
 * with few groups in view shows each as a bar, one row unless they overlap; a
 * track with many shows a stacked column per slice of time. A track that is
 * sent, not played, has no rendered strip.
 */
export function trackLane(
	groups: readonly GroupRecord[],
	renders: boolean,
	now: number,
	span: number,
): Lane {
	const from = now - span;
	const x = (time: number) => Math.min(1, Math.max(0, (time - from) / span));
	const visible = groups.filter((g) => (g.ended ?? now) >= from);
	const shapes: Shape[] = [];

	const dense = visible.length > BAR_LIMIT;
	const bars = dense ? [] : packLanes(visible, now);
	const lanes = Math.min(MAX_LANES, bars.reduce((most, bar) => Math.max(most, bar.lane + 1), 1));
	const networkHeight = dense ? COLUMN_HEIGHT : lanes * (LANE_HEIGHT + LANE_GAP) - LANE_GAP;
	const renderTop = networkHeight + RENDER_GAP;

	if (renders) {
		shapes.push({ x0: 0, x1: 1, y: renderTop, height: RENDER_HEIGHT, style: "track" });
	}

	for (const bar of bars) {
		const group = bar.group;
		shapes.push({
			x0: x(bar.start),
			x1: x(bar.end),
			y: Math.min(bar.lane, MAX_LANES - 1) * (LANE_HEIGHT + LANE_GAP),
			height: LANE_HEIGHT,
			style: group.state,
			title: describeGroup(group, now),
			group: group.sequence,
		});
		if (!renders) continue;

		if (group.renderStart !== undefined) {
			shapes.push({
				x0: x(group.renderStart),
				x1: x(group.renderEnd ?? group.renderStart),
				y: renderTop,
				height: RENDER_HEIGHT,
				style: "rendered",
				title:
					`Group ${group.sequence}: ${group.rendered} of ${group.frames} frames rendered`,
				group: group.sequence,
			});
		} else if (group.state !== "receiving" && group.state !== "stopped") {
			// It ended with nothing shown, which includes a group received in
			// full that playback passed over.
			shapes.push({
				x0: x(group.arrived),
				x1: x(group.arrived),
				y: renderTop,
				height: RENDER_HEIGHT,
				style: "unrendered",
				title: `Group ${group.sequence}: not rendered (${group.state})`,
				group: group.sequence,
			});
		}
	}

	if (dense) {
		const cols = columns(visible, from, now, span / COLUMN_COUNT);
		const tallest = cols.reduce((most, c) => Math.max(most, total(c.states)), 1);
		cols.forEach((col, i) => {
			if (total(col.states) === 0) return;
			const x0 = i / COLUMN_COUNT;
			const x1 = (i + 1) / COLUMN_COUNT;
			const title = describeColumn(col);

			let top = COLUMN_HEIGHT;
			for (const state of STACK_ORDER) {
				if (col.states[state] === 0) continue;
				const height = col.states[state] / tallest * COLUMN_HEIGHT;
				top -= height;
				shapes.push({ x0, x1, y: top, height, style: state, title });
			}
			if (renders && col.frames > 0 && col.rendered > 0) {
				const height = col.rendered / col.frames * RENDER_HEIGHT;
				shapes.push({
					x0,
					x1,
					y: renderTop + RENDER_HEIGHT - height,
					height,
					style: "rendered",
					title,
				});
			}
		});
	}

	return { height: renders ? renderTop + RENDER_HEIGHT : networkHeight, shapes };
}

/**
 * The audio output's lane: how low its buffer got over time, and a mark at
 * each moment it put out silence or threw audio away. Those marks are the
 * crackles: the track lanes show what the network delivered, this one shows
 * what reached the speaker.
 *
 * Two thin strips along the top say why the buffer was not being filled: the
 * upper one where no audio was arriving, the lower one where audio that had
 * arrived was waiting in the player for an earlier group. A glitch under
 * neither was caused after the audio was handed to the decoder.
 */
export function audioLane(
	history: readonly AudioBufferSample[],
	delays: readonly DelayRecord[],
	now: number,
	span: number,
): Lane {
	const from = now - span;
	const x = (time: number) => Math.min(1, Math.max(0, (time - from) / span));
	const visible = history.filter((s) => s.at >= from);
	const ceiling = visible.reduce((most, s) => Math.max(most, s.buffered), MIN_AUDIO_CEILING_MS);

	const strips = delays.filter((d) => d.track === "audio" && d.at >= from).map((d): Shape => ({
		x0: x(d.at - d.duration),
		x1: x(d.at),
		y: d.kind === "arrival" ? 0 : DELAY_HEIGHT,
		height: DELAY_HEIGHT,
		style: d.kind,
		title: d.kind === "arrival"
			? `No audio arrived for ${Math.round(d.duration)} ms`
			: `Audio waited ${Math.round(d.duration)} ms in the player for an earlier group`,
	}));

	return {
		height: AUDIO_HEIGHT,
		// The bottom of each swing, not the level at the moment of the report.
		level: visible.map((s) => [x(s.at), AUDIO_HEIGHT - s.low / ceiling * AUDIO_HEIGHT]),
		shapes: [
			...audioGlitches(history).filter((g) => g.at >= from).map((glitch): Shape => ({
				x0: x(glitch.at),
				x1: x(glitch.at),
				y: 0,
				height: AUDIO_HEIGHT,
				style: "glitch",
				title: describeGlitch(glitch),
			})),
			...strips,
		],
	};
}

const STALL_HEIGHT = 8;

/**
 * The main thread's lane: a mark as long as each stop. A stop under an audio
 * glitch is a glitch the page caused; a glitch with nothing above it came from
 * the stream.
 */
export function stallLane(stalls: readonly StallRecord[], now: number, span: number): Lane {
	const from = now - span;
	const x = (time: number) => Math.min(1, Math.max(0, (time - from) / span));
	return {
		height: STALL_HEIGHT,
		shapes: [
			{ x0: 0, x1: 1, y: 0, height: STALL_HEIGHT, style: "track" },
			...stalls.filter((s) => s.at >= from).map((s): Shape => ({
				x0: x(s.at - s.duration),
				x1: x(s.at),
				y: 0,
				height: STALL_HEIGHT,
				style: "stall",
				title: `Main thread stopped for ${Math.round(s.duration)} ms`,
			})),
		],
	};
}

/** The topmost shape with something to say under a point, if any. */
export function shapeAt(
	lane: Lane,
	x: number,
	y: number,
	width: number,
	minWidth: number,
): Shape | undefined {
	for (let i = lane.shapes.length - 1; i >= 0; i--) {
		const shape = lane.shapes[i];
		if (shape?.title === undefined) continue;
		const left = shape.x0 * width;
		const right = Math.max(shape.x1 * width, left + minWidth);
		if (x >= left && x <= right && y >= shape.y && y <= shape.y + shape.height) return shape;
	}
	return undefined;
}

export function formatBytes(bytes: number): string {
	if (bytes >= 1_000_000) return `${(bytes / 1_000_000).toFixed(1)} MB`;
	if (bytes >= 1000) return `${(bytes / 1000).toFixed(1)} kB`;
	return `${bytes} B`;
}

function total(states: Readonly<Record<GroupState, number>>): number {
	return STACK_ORDER.reduce((sum, state) => sum + states[state], 0);
}

function describeGroup(group: GroupRecord, now: number): string {
	const duration = Math.round((group.ended ?? now) - group.arrived);
	return [
		`Group ${group.sequence}: ${group.state}`,
		`${group.frames} frames, ${formatBytes(group.bytes)}`,
		`${duration} ms on the wire`,
		`${group.rendered} rendered`,
	].join(" · ");
}

function describeColumn(col: Column): string {
	const parts = STACK_ORDER.filter((state) => col.states[state] > 0)
		.map((state) => `${col.states[state]} ${state}`);
	return `${total(col.states)} groups: ${
		parts.join(", ")
	} · ${col.rendered} of ${col.frames} frames rendered`;
}

function describeGlitch(glitch: AudioGlitch): string {
	const causes: [number, string][] = [
		[glitch.starved, "silent: the buffer ran dry"],
		[glitch.gaps, "silent: audio was missing"],
		[glitch.late, "dropped: arrived too late"],
		[glitch.overflowed, "dropped: more arrived than fits"],
		[glitch.trimmed, "skipped: buffered but never needed"],
	];
	return causes.filter(([ms]) => ms >= GLITCH_MS)
		.map(([ms, cause]) => `${Math.round(ms)} ms ${cause}`)
		.join(" · ");
}
