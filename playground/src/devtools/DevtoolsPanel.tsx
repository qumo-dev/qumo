import { createEffect, createMemo, createSignal, For, onCleanup, Show } from "solid-js";
import type { Session } from "@qumo/moq";
import type { GroupRecord, GroupState, Recorder, TrackRecord } from "./recorder.ts";
import { BAR_LIMIT, columns, packLanes } from "./timeline.ts";

const STORAGE_KEY = "qumo.devtools.open";

// Rates are measured over at least this long, however often the panel redraws.
const RATE_MS = 1000;
// How much time the timeline can show. The shorter spans are what make a busy
// track readable: at 60 s audio is tens of groups a second and is drawn as
// columns, at 2 s each of its groups is a bar on its own lane.
const SPANS = [60_000, 10_000, 2_000] as const;
type Span = typeof SPANS[number];
// A short span scrolls too fast to follow at one redraw a second.
const TICK_MS: Record<Span, number> = { 60_000: 1000, 10_000: 250, 2_000: 100 };

// Timeline geometry, in SVG user units. Horizontally the span is always WIDTH
// units and the drawing stretches to the panel's width; vertically one unit is
// one pixel.
const WIDTH = 600;
const COLUMNS = 60;
const LANE_HEIGHT = 10;
const LANE_GAP = 2;
const MAX_LANES = 4;
const RENDER_HEIGHT = 6;
const RENDER_GAP = 5;
const COLUMN_HEIGHT = 22;
// Narrowest a mark is drawn, so a 20 ms group is still visible.
const MIN_WIDTH = 1.5;
// Shaved off each mark so neighbours are separated by the surface, not a stroke.
const GAP = 0.8;

const STATE_LABELS: Record<GroupState, string> = {
	receiving: "Receiving",
	complete: "Complete",
	skipped: "Skipped",
	aborted: "Aborted",
	late: "Late",
	stopped: "Stopped",
};
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

interface TrackRates {
	/** Received media, in bits per second. */
	readonly bitrate: number;
	readonly framesPerSecond: number;
}

interface SessionReading {
	readonly rtt: number;
	readonly estimatedBitrate: number;
	readonly bytesSent: number;
	readonly bytesReceived: number;
	/** Bits per second over the last tick; undefined on the first reading. */
	readonly sendRate: number | undefined;
	readonly receiveRate: number | undefined;
}

interface Reading {
	readonly now: number;
	readonly tracks: readonly TrackRecord[];
	readonly rates: ReadonlyMap<string, TrackRates>;
}

// The group the pointer is on, so its received bar and its rendered span can
// be picked out together.
interface Focus {
	readonly track: string;
	readonly sequence: number;
}

/**
 * Collapsible panel showing what the session's tracks and groups are doing.
 * The recorder runs whether or not the panel is open; the panel only reads it,
 * while open and not paused.
 */
export function DevtoolsPanel(props: { recorder: Recorder; session: Promise<Session> }) {
	const [open, setOpen] = createSignal(readOpen());
	const [reading, setReading] = createSignal<Reading>({ now: 0, tracks: [], rates: new Map() });
	const [session, setSession] = createSignal<SessionReading>();
	const [span, setSpan] = createSignal<Span>(60_000);
	const [paused, setPaused] = createSignal(false);
	const [focus, setFocus] = createSignal<Focus>();

	// What the rates are measured against. Kept across redraw-rate changes so
	// zooming or resuming does not blank them.
	let baseline: Reading | undefined;
	let rates: ReadonlyMap<string, TrackRates> = new Map();
	let previousSession: { at: number; reading: SessionReading } | undefined;

	const toggle = () => {
		const next = !open();
		setOpen(next);
		writeOpen(next);
	};

	createEffect(() => {
		if (!open() || paused()) return;

		let stopped = false;

		const sample = () => {
			const now = performance.now();
			const tracks = props.recorder.snapshot();
			const due = baseline === undefined || now - baseline.now >= RATE_MS;
			if (due) rates = ratesSince(baseline, now, tracks);
			const next = { now, tracks, rates };
			if (due) baseline = next;
			setReading(next);
			if (!due) return;

			void props.session.then((s) => s.getStats()).then((stats) => {
				if (stopped) return;
				const seconds = previousSession ? (now - previousSession.at) / 1000 : 0;
				const before = previousSession?.reading;
				const reading: SessionReading = {
					...stats,
					sendRate: before && seconds > 0
						? (stats.bytesSent - before.bytesSent) * 8 / seconds
						: undefined,
					receiveRate: before && seconds > 0
						? (stats.bytesReceived - before.bytesReceived) * 8 / seconds
						: undefined,
				};
				previousSession = { at: now, reading };
				setSession(reading);
				// reason: stats are unavailable until the session connects, and
				// again once it closes; the panel just shows no session row.
			}, () => {});
		};

		sample();
		const timer = setInterval(sample, TICK_MS[span()]);
		onCleanup(() => {
			stopped = true;
			clearInterval(timer);
		});
	});

	return (
		<section class="devtools">
			<button
				type="button"
				class="devtools-toggle"
				aria-expanded={open()}
				onClick={toggle}
			>
				<span class="devtools-toggle-mark" aria-hidden="true">{open() ? "▾" : "▸"}</span>
				DevTools
			</button>

			<Show when={open()}>
				<div class="devtools-body">
					<SessionRow reading={session()} media={mediaRate(reading())} />

					<Show
						when={reading().tracks.length > 0}
						fallback={
							<p class="devtools-empty">
								No tracks yet. Groups appear here once you start publishing or
								subscribing.
							</p>
						}
					>
						<TrackTable reading={reading()} />
						<Timeline
							reading={reading()}
							span={span()}
							onSpan={setSpan}
							paused={paused()}
							onPause={() => setPaused((p) => !p)}
							focus={focus()}
							onFocus={setFocus}
						/>
					</Show>
				</div>
			</Show>
		</section>
	);
}

// The transport's own counters are shown only when it reports them: a browser
// without WebTransport statistics returns zeros, which would read as "nothing
// is flowing". The media rate comes from the recorder, so it is always there.
function SessionRow(props: { reading: SessionReading | undefined; media: number }) {
	return (
		<dl class="devtools-session">
			<div>
				<dt>Media received</dt>
				<dd>{formatBitrate(props.media)}</dd>
			</div>
			<Show when={(props.reading?.rtt ?? 0) > 0}>
				<div>
					<dt>RTT</dt>
					<dd>{Math.round(props.reading?.rtt ?? 0)} ms</dd>
				</div>
			</Show>
			<Show when={(props.reading?.bytesReceived ?? 0) > 0}>
				<div>
					<dt>Connection received</dt>
					<dd>
						{formatBytes(props.reading?.bytesReceived ?? 0)}
						<Show when={props.reading?.receiveRate !== undefined}>
							{` (${formatBitrate(props.reading?.receiveRate ?? 0)})`}
						</Show>
					</dd>
				</div>
			</Show>
			<Show when={(props.reading?.bytesSent ?? 0) > 0}>
				<div>
					<dt>Connection sent</dt>
					<dd>
						{formatBytes(props.reading?.bytesSent ?? 0)}
						<Show when={props.reading?.sendRate !== undefined}>
							{` (${formatBitrate(props.reading?.sendRate ?? 0)})`}
						</Show>
					</dd>
				</div>
			</Show>
			<Show when={(props.reading?.estimatedBitrate ?? 0) > 0}>
				<div>
					<dt>Estimated send rate</dt>
					<dd>{formatBitrate(props.reading?.estimatedBitrate ?? 0)}</dd>
				</div>
			</Show>
		</dl>
	);
}

// Bits per second of media received across the played tracks over the last
// tick. Sent tracks are recorded too, and are not counted here.
function mediaRate(reading: Reading): number {
	let sum = 0;
	for (const track of reading.tracks) {
		if (track.renders) sum += reading.rates.get(track.name)?.bitrate ?? 0;
	}
	return sum;
}

function TrackTable(props: { reading: Reading }) {
	return (
		<div class="devtools-scroll">
			<table class="devtools-tracks">
				<thead>
					<tr>
						<th scope="col">Track</th>
						<th scope="col">Bitrate</th>
						<th scope="col">Frames/s</th>
						<th scope="col">Latest group</th>
						<th scope="col">Complete</th>
						<th scope="col">Skipped</th>
						<th scope="col">Aborted</th>
						<th scope="col">Late</th>
					</tr>
				</thead>
				<tbody>
					<For each={props.reading.tracks}>
						{(track) => {
							const rates = () => props.reading.rates.get(track.name);
							return (
								<tr>
									<th scope="row">{track.name}</th>
									<td>{formatBitrate(rates()?.bitrate ?? 0)}</td>
									<td>{Math.round(rates()?.framesPerSecond ?? 0)}</td>
									<td>{track.latest ?? "—"}</td>
									<td>{track.ended.complete}</td>
									<td>{track.ended.skipped}</td>
									<td>{track.ended.aborted}</td>
									<td>{track.ended.late}</td>
								</tr>
							);
						}}
					</For>
				</tbody>
			</table>
		</div>
	);
}

function Timeline(props: {
	reading: Reading;
	span: Span;
	onSpan: (span: Span) => void;
	paused: boolean;
	onPause: () => void;
	focus: Focus | undefined;
	onFocus: (focus: Focus | undefined) => void;
}) {
	return (
		<div class="devtools-timeline">
			<div class="devtools-controls">
				<div class="devtools-spans" role="group" aria-label="Time shown">
					<For each={SPANS}>
						{(ms) => (
							<button
								type="button"
								class="copy-btn"
								aria-pressed={props.span === ms}
								onClick={() => props.onSpan(ms)}
							>
								{ms / 1000} s
							</button>
						)}
					</For>
				</div>
				<button
					type="button"
					class="copy-btn"
					aria-pressed={props.paused}
					onClick={() => props.onPause()}
				>
					{props.paused ? "Resume" : "Pause"}
				</button>
			</div>

			<For each={props.reading.tracks}>
				{(track) => (
					<TrackLanes
						track={track}
						now={props.reading.now}
						span={props.span}
						focus={props.focus?.track === track.name ? props.focus.sequence : undefined}
						onFocus={(sequence) =>
							props.onFocus(
								sequence === undefined
									? undefined
									: { track: track.name, sequence },
							)}
					/>
				)}
			</For>

			<div class="devtools-lane devtools-axis" aria-hidden="true">
				<span />
				<div>
					<span>−{props.span / 1000} s</span>
					<span>−{props.span / 2000} s</span>
					<span>now</span>
				</div>
			</div>

			<ul class="devtools-legend">
				<For each={STACK_ORDER}>
					{(state) => (
						<li>
							<span class={`devtools-swatch is-${state}`} />
							{STATE_LABELS[state]}
						</li>
					)}
				</For>
				<li>
					<span class="devtools-swatch is-rendered" />
					Rendered
				</li>
			</ul>
		</div>
	);
}

// One track: its groups as they arrived (the network view) above a single lane
// of what was rendered, on the same time axis. Pointing at a group picks out
// both its bar and its rendered span.
function TrackLanes(props: {
	track: TrackRecord;
	now: number;
	span: Span;
	focus: number | undefined;
	onFocus: (sequence: number | undefined) => void;
}) {
	const from = () => props.now - props.span;
	const x = (time: number) => Math.max(0, (time - from()) * WIDTH / props.span);
	const visible = createMemo(() =>
		props.track.groups.filter((g) => (g.ended ?? props.now) >= from())
	);
	const dense = () => visible().length > BAR_LIMIT;

	const bars = createMemo(() => dense() ? [] : packLanes(visible(), props.now));
	const laneCount = () =>
		Math.min(MAX_LANES, bars().reduce((most, bar) => Math.max(most, bar.lane + 1), 1));
	const networkHeight = () =>
		dense() ? COLUMN_HEIGHT : laneCount() * (LANE_HEIGHT + LANE_GAP) - LANE_GAP;
	const renderTop = () => networkHeight() + RENDER_GAP;
	// A sent track has nothing to render, so it gets no rendered lane.
	const height = () => props.track.renders ? renderTop() + RENDER_HEIGHT : networkHeight();
	const hatch = () => `devtools-hatch-${props.track.name.replace(/\W+/g, "-")}`;

	return (
		<div class="devtools-lane">
			<span class="devtools-lane-name">{props.track.name}</span>
			<svg
				class="devtools-plot"
				classList={{ "has-focus": props.focus !== undefined }}
				viewBox={`0 0 ${WIDTH} ${height()}`}
				height={height()}
				preserveAspectRatio="none"
				role="img"
				aria-label={`${props.track.name}: groups over the last ${
					props.span / 1000
				} seconds`}
			>
				<defs>
					<pattern
						id={hatch()}
						width="3"
						height="3"
						patternUnits="userSpaceOnUse"
						patternTransform="rotate(45)"
					>
						<rect class="devtools-hatch-fill" width="1.5" height="3" />
					</pattern>
				</defs>
				<Show when={props.track.renders}>
					<rect
						class="devtools-render-track"
						x="0"
						y={renderTop()}
						width={WIDTH}
						height={RENDER_HEIGHT}
					/>
				</Show>

				<Show
					when={dense()}
					fallback={
						<For each={bars()}>
							{(bar) => {
								const lane = Math.min(bar.lane, MAX_LANES - 1);
								return (
									<>
										<rect
											class={`devtools-mark is-${bar.group.state}`}
											classList={{
												"is-linked": props.focus === bar.group.sequence,
											}}
											onMouseEnter={() => props.onFocus(bar.group.sequence)}
											onMouseLeave={() => props.onFocus(undefined)}
											fill={bar.group.state === "skipped"
												? `url(#${hatch()})`
												: undefined}
											x={x(bar.start)}
											y={lane * (LANE_HEIGHT + LANE_GAP)}
											width={markWidth(x(bar.end) - x(bar.start))}
											height={LANE_HEIGHT}
										>
											<title>{describeGroup(bar.group, props.now)}</title>
										</rect>
										<Show when={props.track.renders}>
											<RenderMark
												group={bar.group}
												x={x}
												top={renderTop()}
												linked={props.focus === bar.group.sequence}
												onFocus={props.onFocus}
											/>
										</Show>
									</>
								);
							}}
						</For>
					}
				>
					<DenseColumns
						groups={visible()}
						from={from()}
						now={props.now}
						size={props.span / COLUMNS}
						hatch={hatch()}
						renderTop={props.track.renders ? renderTop() : undefined}
					/>
				</Show>
			</svg>
		</div>
	);
}

// What the rendered lane shows for one group: the span over which its frames
// reached the output, or a tick where it arrived if it ended with none shown.
// That includes a group received in full that playback passed over.
function RenderMark(props: {
	group: GroupRecord;
	x: (time: number) => number;
	top: number;
	linked: boolean;
	onFocus: (sequence: number | undefined) => void;
}) {
	const unrendered = () =>
		props.group.rendered === 0 && props.group.state !== "receiving" &&
		props.group.state !== "stopped";

	return (
		<>
			<Show when={props.group.renderStart !== undefined}>
				<rect
					class="devtools-mark is-rendered"
					classList={{ "is-linked": props.linked }}
					onMouseEnter={() => props.onFocus(props.group.sequence)}
					onMouseLeave={() => props.onFocus(undefined)}
					x={props.x(props.group.renderStart ?? 0)}
					y={props.top}
					width={markWidth(
						props.x(props.group.renderEnd ?? 0) - props.x(props.group.renderStart ?? 0),
					)}
					height={RENDER_HEIGHT}
				>
					<title>
						{`Group ${props.group.sequence}: ${props.group.rendered} of ${props.group.frames} frames rendered`}
					</title>
				</rect>
			</Show>
			<Show when={unrendered()}>
				<rect
					class="devtools-mark is-unrendered"
					classList={{ "is-linked": props.linked }}
					x={props.x(props.group.arrived)}
					y={props.top}
					width={MIN_WIDTH}
					height={RENDER_HEIGHT}
				>
					<title>
						{`Group ${props.group.sequence}: not rendered (${props.group.state})`}
					</title>
				</rect>
			</Show>
		</>
	);
}

// A track with too many groups to draw one by one: a column per slice of time,
// stacked by how its groups ended, and below it the share of their frames
// rendered.
function DenseColumns(props: {
	groups: readonly GroupRecord[];
	from: number;
	now: number;
	/** Milliseconds each column covers. */
	size: number;
	hatch: string;
	/** Top of the rendered lane; undefined for a track that renders nothing. */
	renderTop: number | undefined;
}) {
	const cols = createMemo(() => columns(props.groups, props.from, props.now, props.size));
	const tallest = () => cols().reduce((most, c) => Math.max(most, total(c.states)), 1);
	const width = WIDTH / COLUMNS;

	return (
		<For each={cols()}>
			{(col, i) => {
				const groups = total(col.states);
				const scale = COLUMN_HEIGHT / tallest();
				let top = COLUMN_HEIGHT;
				const segments = STACK_ORDER.filter((state) => col.states[state] > 0).map(
					(state) => {
						const h = col.states[state] * scale;
						top -= h;
						return { state, y: top, h };
					},
				);
				const share = col.frames > 0 ? col.rendered / col.frames : 0;

				return (
					<Show when={groups > 0}>
						<g>
							<title>{describeColumn(col.states, col.rendered, col.frames)}</title>
							<For each={segments}>
								{(seg) => (
									<rect
										class={`devtools-mark is-${seg.state}`}
										fill={seg.state === "skipped"
											? `url(#${props.hatch})`
											: undefined}
										x={i() * width}
										y={seg.y}
										width={width - GAP}
										height={seg.h}
									/>
								)}
							</For>
							<Show when={props.renderTop}>
								{(renderTop) => (
									<rect
										class="devtools-mark is-rendered"
										x={i() * width}
										y={renderTop() + RENDER_HEIGHT * (1 - share)}
										width={width - GAP}
										height={RENDER_HEIGHT * share}
									/>
								)}
							</Show>
						</g>
					</Show>
				);
			}}
		</For>
	);
}

function ratesSince(
	previous: Reading | undefined,
	now: number,
	tracks: readonly TrackRecord[],
): Map<string, TrackRates> {
	const rates = new Map<string, TrackRates>();
	if (previous === undefined) return rates;
	const seconds = (now - previous.now) / 1000;
	if (!(seconds > 0)) return rates;

	for (const track of tracks) {
		const before = previous.tracks.find((t) => t.name === track.name);
		rates.set(track.name, {
			bitrate: (track.bytes - (before?.bytes ?? 0)) * 8 / seconds,
			framesPerSecond: (track.frames - (before?.frames ?? 0)) / seconds,
		});
	}
	return rates;
}

function total(states: Readonly<Record<GroupState, number>>): number {
	return STACK_ORDER.reduce((sum, state) => sum + states[state], 0);
}

function markWidth(width: number): number {
	return Math.max(MIN_WIDTH, width - GAP);
}

function describeGroup(group: GroupRecord, now: number): string {
	const duration = Math.round((group.ended ?? now) - group.arrived);
	return [
		`Group ${group.sequence}: ${STATE_LABELS[group.state].toLowerCase()}`,
		`${group.frames} frames, ${formatBytes(group.bytes)}`,
		`${duration} ms on the wire`,
		`${group.rendered} rendered`,
	].join(" · ");
}

function describeColumn(
	states: Readonly<Record<GroupState, number>>,
	rendered: number,
	frames: number,
): string {
	const parts = STACK_ORDER.filter((state) => states[state] > 0)
		.map((state) => `${states[state]} ${STATE_LABELS[state].toLowerCase()}`);
	return `${total(states)} groups: ${
		parts.join(", ")
	} · ${rendered} of ${frames} frames rendered`;
}

function formatBitrate(bitsPerSecond: number): string {
	if (bitsPerSecond >= 1_000_000) return `${(bitsPerSecond / 1_000_000).toFixed(2)} Mbps`;
	return `${Math.round(bitsPerSecond / 1000)} kbps`;
}

function formatBytes(bytes: number): string {
	if (bytes >= 1_000_000) return `${(bytes / 1_000_000).toFixed(1)} MB`;
	if (bytes >= 1000) return `${(bytes / 1000).toFixed(1)} kB`;
	return `${bytes} B`;
}

// The panel stays closed unless the viewer opened it before. Storage can be
// unavailable (private mode, blocked site data); the panel works without it.
function readOpen(): boolean {
	try {
		return localStorage.getItem(STORAGE_KEY) === "1";
	} catch {
		// reason: no storage means no remembered choice, which is the default.
		return false;
	}
}

function writeOpen(open: boolean): void {
	try {
		localStorage.setItem(STORAGE_KEY, open ? "1" : "0");
	} catch {
		// reason: the choice just isn't remembered.
	}
}
