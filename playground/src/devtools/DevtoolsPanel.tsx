import {
	createEffect,
	createMemo,
	createSignal,
	For,
	Index,
	onCleanup,
	onMount,
	Show,
} from "solid-js";
import type { Session } from "@qumo/moq";
import type {
	AudioBufferRecord,
	AudioBufferSample,
	DelayRecord,
	PlaybackTimingRecord,
	Recorder,
	StallRecord,
	TrackRecord,
} from "./recorder.ts";
import {
	audioLane,
	clockRates,
	formatBytes,
	type Lane,
	shapeAt,
	type ShapeStyle,
	stallLane,
	trackLane,
} from "./timeline.ts";

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
	/** The audio jitter buffer's reports, oldest first; empty until audio has played. */
	readonly audio: readonly AudioBufferSample[];
	readonly timing: PlaybackTimingRecord | undefined;
	/** The main thread's recent stops, oldest first. */
	readonly stalls: readonly StallRecord[];
	/** Recent stretches in which a track's media was not moving, oldest first. */
	readonly delays: readonly DelayRecord[];
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
	const [reading, setReading] = createSignal<Reading>({
		now: 0,
		tracks: [],
		rates: new Map(),
		audio: [],
		timing: undefined,
		stalls: [],
		delays: [],
	});
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
			const next = {
				now,
				tracks,
				rates,
				audio: props.recorder.audio(),
				timing: props.recorder.timing(),
				stalls: props.recorder.stalls(),
				delays: props.recorder.delays(),
			};
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
					<SessionRow
						reading={session()}
						media={mediaRate(reading())}
						timing={reading().timing}
						stalls={reading().stalls}
					/>
					<Show when={reading().audio.at(-1)}>
						{(audio) => (
							<AudioBufferRow
								audio={audio()}
								history={reading().audio}
								delays={reading().delays}
							/>
						)}
					</Show>

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
function SessionRow(props: {
	reading: SessionReading | undefined;
	media: number;
	timing: PlaybackTimingRecord | undefined;
	stalls: readonly StallRecord[];
}) {
	const longest = () => props.stalls.reduce((most, s) => Math.max(most, s.duration), 0);

	return (
		<dl class="devtools-session">
			<div>
				<dt>Media received</dt>
				<dd>{formatBitrate(props.media)}</dd>
			</div>
			<Show when={props.timing}>
				{(timing) => (
					<>
						<div title="How far playback trails the live edge. It grows to cover the arrival jitter, and when the audio buffer runs dry.">
							<dt>Playback delay</dt>
							<dd>{Math.round(timing().delay)} ms</dd>
						</div>
						<div title="How late media has recently arrived against its fastest arrival. The delay has to cover this.">
							<dt>Arrival jitter</dt>
							<dd>
								{`audio ${Math.round(timing().audioJitter)} ms, video ${
									Math.round(timing().videoJitter)
								} ms`}
							</dd>
						</div>
					</>
				)}
			</Show>
			<div title="How often, and for how long at most, the page's main thread stopped in the last minute. Media passes through it, so a stop longer than the audio buffer is a gap in the sound.">
				<dt>Main thread stops</dt>
				<dd>
					{props.stalls.length === 0
						? "none"
						: `${props.stalls.length}, longest ${Math.round(longest())} ms`}
				</dd>
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

// The audio jitter buffer: how much is waiting to be played, and every way it
// has had to put silence out or throw audio away. Any of the counts rising is
// audible; all of them standing still is what healthy playback looks like.
function AudioBufferRow(props: {
	audio: AudioBufferRecord;
	history: readonly AudioBufferSample[];
	delays: readonly DelayRecord[];
}) {
	const rates = createMemo(() => clockRates(props.history));
	// How often, and for how long at most, audio was kept from the buffer in
	// the last minute, in one of the two ways the player can see.
	const delayed = (kind: DelayRecord["kind"]) => {
		const found = props.delays.filter((d) => d.track === "audio" && d.kind === kind);
		if (found.length === 0) return "none";
		const longest = found.reduce((most, d) => Math.max(most, d.duration), 0);
		return `${found.length}, longest ${Math.round(longest)} ms`;
	};
	const percent = (rate: number) => `${(rate * 100).toFixed(1)}%`;

	return (
		<dl class="devtools-session">
			<div>
				<dt>Audio buffer</dt>
				<dd>
					{`${Math.round(props.audio.buffered)} ms of ${
						Math.round(props.audio.latency)
					} ms`}
					{props.audio.stalled ? " (refilling)" : ""}
				</dd>
			</div>
			<div>
				<dt>Ran dry</dt>
				<dd>
					{`${props.audio.underruns}× (${Math.round(props.audio.starved)} ms silent)`}
				</dd>
			</div>
			<div>
				<dt>Missing</dt>
				<dd>{Math.round(props.audio.gaps)} ms</dd>
			</div>
			<div>
				<dt>Too late</dt>
				<dd>{Math.round(props.audio.late)} ms</dd>
			</div>
			<div>
				<dt>Overflowed</dt>
				<dd>{Math.round(props.audio.overflowed)} ms</dd>
			</div>
			<div>
				<dt>Trimmed</dt>
				<dd>{Math.round(props.audio.trimmed)} ms</dd>
			</div>
			<div title="Stretches of 120 ms or more in which no audio came off the transport.">
				<dt>Not arriving</dt>
				<dd>{delayed("arrival")}</dd>
			</div>
			<div title="Times audio that had arrived waited 20 ms or more in the player for an earlier group.">
				<dt>Held back</dt>
				<dd>{delayed("held")}</dd>
			</div>
			<Show when={rates()}>
				{(r) => (
					<div title="How fast each end of the audio buffer runs, against real time. They should match.">
						<dt>Clocks</dt>
						<dd>{`media ${percent(r().media)}, output ${percent(r().output)}`}</dd>
					</div>
				)}
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
					<Index each={props.reading.tracks}>
						{(track) => {
							const rates = () => props.reading.rates.get(track().name);
							return (
								<tr>
									<th scope="row">{track().name}</th>
									<td>{formatBitrate(rates()?.bitrate ?? 0)}</td>
									<td>{Math.round(rates()?.framesPerSecond ?? 0)}</td>
									<td>{track().latest ?? "—"}</td>
									<td>{track().ended.complete}</td>
									<td>{track().ended.skipped}</td>
									<td>{track().ended.aborted}</td>
									<td>{track().ended.late}</td>
								</tr>
							);
						}}
					</Index>
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
	// The marks are painted in the page's theme tokens. Reading them with each
	// new reading keeps the canvases right when the theme changes.
	const palette = createMemo(() => {
		props.reading;
		return readPalette();
	});

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

			{
				/* Index, not For: the records are new objects on every reading, and
			    a lane's canvas should be redrawn, not replaced. */
			}
			<Index each={props.reading.tracks}>
				{(track) => (
					<LaneCanvas
						name={track().name}
						label={`${track().name}: groups over the last ${props.span / 1000} seconds`}
						lane={trackLane(
							track().groups,
							track().renders,
							props.reading.now,
							props.span,
						)}
						palette={palette()}
						focus={props.focus?.track === track().name
							? props.focus.sequence
							: undefined}
						onFocus={(sequence) => props.onFocus(
							sequence === undefined ? undefined : { track: track().name, sequence },
						)}
					/>
				)}
			</Index>

			<Show when={props.reading.audio.length > 0}>
				<LaneCanvas
					name="audio out"
					label={`Audio output: buffer level and glitches over the last ${
						props.span / 1000
					} seconds`}
					lane={audioLane(
						props.reading.audio,
						props.reading.delays,
						props.reading.now,
						props.span,
					)}
					palette={palette()}
					focus={undefined}
				/>
			</Show>

			<LaneCanvas
				name="main thread"
				label={`Main thread: ${props.reading.stalls.length} stops over the last ${
					props.span / 1000
				} seconds`}
				lane={stallLane(props.reading.stalls, props.reading.now, props.span)}
				palette={palette()}
				focus={undefined}
			/>

			<div class="devtools-lane devtools-axis" aria-hidden="true">
				<span />
				<div>
					<span>−{props.span / 1000} s</span>
					<span>−{props.span / 2000} s</span>
					<span>now</span>
				</div>
			</div>

			<ul class="devtools-legend">
				<For each={LEGEND}>
					{([style, label]) => (
						<li>
							<span class={`devtools-swatch is-${style}`} />
							{label}
						</li>
					)}
				</For>
			</ul>
		</div>
	);
}

// One lane of the timeline, drawn on a canvas. Pointing at a mark shows what
// it is, and picks out the other marks of the same group.
function LaneCanvas(props: {
	name: string;
	label: string;
	lane: Lane;
	palette: Palette;
	focus: number | undefined;
	onFocus?: (group: number | undefined) => void;
}) {
	let canvas: HTMLCanvasElement | undefined;
	const [width, setWidth] = createSignal(0);
	const lane = createMemo(() => props.lane);
	let pointed: number | undefined;

	onMount(() => {
		if (!canvas) return;
		const observer = new ResizeObserver(([entry]) => setWidth(entry?.contentRect.width ?? 0));
		observer.observe(canvas);
		onCleanup(() => observer.disconnect());
	});

	createEffect(() => {
		if (canvas && width() > 0) drawLane(canvas, lane(), width(), props.palette, props.focus);
	});

	const point = (event: MouseEvent) => {
		if (!canvas) return;
		const box = canvas.getBoundingClientRect();
		const hit = shapeAt(
			lane(),
			event.clientX - box.left,
			event.clientY - box.top,
			box.width,
			POINTER_WIDTH,
		);
		canvas.title = hit?.title ?? "";
		if (hit?.group === pointed) return;
		pointed = hit?.group;
		props.onFocus?.(pointed);
	};

	const leave = () => {
		if (canvas) canvas.title = "";
		if (pointed === undefined) return;
		pointed = undefined;
		props.onFocus?.(undefined);
	};

	return (
		<div class="devtools-lane">
			<span class="devtools-lane-name">{props.name}</span>
			<canvas
				ref={(element) => {
					canvas = element;
				}}
				class="devtools-plot"
				role="img"
				aria-label={props.label}
				style={{ height: `${lane().height}px` }}
				onMouseMove={point}
				onMouseLeave={leave}
			/>
		</div>
	);
}

// Narrowest a mark is drawn, in pixels, so a 20 ms group is still visible.
const MIN_WIDTH = 2;
// Shaved off each mark so neighbours are separated by the surface, not a stroke.
const GAP = 1;
// How wide a mark is to the pointer at least: easier to hit than to see.
const POINTER_WIDTH = 5;

// The theme colours the marks are painted in.
interface Palette {
	readonly complete: string;
	readonly receiving: string;
	readonly stopped: string;
	readonly skipped: string;
	readonly aborted: string;
	readonly late: string;
	readonly rendered: string;
	readonly surface: string;
}

function readPalette(): Palette {
	const style = getComputedStyle(document.documentElement);
	const token = (name: string) => style.getPropertyValue(name).trim();
	return {
		complete: token("--group-complete"),
		receiving: token("--group-receiving"),
		stopped: token("--group-stopped"),
		skipped: token("--group-skipped"),
		aborted: token("--group-aborted"),
		late: token("--group-late"),
		rendered: token("--group-rendered"),
		surface: token("--surface-2"),
	};
}

// How each kind of mark is painted. The states that mean something went wrong
// differ in more than colour: skipped is hatched and late is an outline, so
// they can be told apart without telling the colours apart.
const PAINT: Record<
	ShapeStyle,
	{ color: keyof Palette; alpha: number; fill: "solid" | "hatch" | "outline" }
> = {
	complete: { color: "complete", alpha: 0.55, fill: "solid" },
	receiving: { color: "receiving", alpha: 1, fill: "solid" },
	stopped: { color: "stopped", alpha: 0.25, fill: "solid" },
	skipped: { color: "skipped", alpha: 1, fill: "hatch" },
	aborted: { color: "aborted", alpha: 1, fill: "solid" },
	late: { color: "late", alpha: 1, fill: "outline" },
	rendered: { color: "rendered", alpha: 1, fill: "solid" },
	unrendered: { color: "skipped", alpha: 1, fill: "solid" },
	glitch: { color: "aborted", alpha: 1, fill: "solid" },
	stall: { color: "skipped", alpha: 1, fill: "solid" },
	arrival: { color: "late", alpha: 1, fill: "solid" },
	held: { color: "skipped", alpha: 1, fill: "hatch" },
	track: { color: "surface", alpha: 1, fill: "solid" },
};

const LEGEND: readonly (readonly [string, string])[] = [
	["complete", "Complete"],
	["receiving", "Receiving"],
	["stopped", "Stopped"],
	["skipped", "Skipped"],
	["aborted", "Aborted"],
	["late", "Late"],
	["rendered", "Rendered"],
	["glitch", "Audio glitch"],
	["stall", "Main thread stopped"],
	["arrival", "No audio arriving"],
	["held", "Audio held back"],
];

function drawLane(
	canvas: HTMLCanvasElement,
	lane: Lane,
	width: number,
	palette: Palette,
	focus: number | undefined,
): void {
	// Size the bitmap in device pixels so marks stay sharp, and draw in CSS pixels.
	const ratio = devicePixelRatio || 1;
	const bitmapWidth = Math.round(width * ratio);
	const bitmapHeight = Math.round(lane.height * ratio);
	if (canvas.width !== bitmapWidth) canvas.width = bitmapWidth;
	if (canvas.height !== bitmapHeight) canvas.height = bitmapHeight;

	const context = canvas.getContext("2d");
	if (context === null) return;
	context.setTransform(ratio, 0, 0, ratio, 0, 0);
	context.clearRect(0, 0, width, lane.height);

	const first = lane.level?.[0];
	const last = lane.level?.at(-1);
	if (lane.level && first && last) {
		context.globalAlpha = 0.35;
		context.fillStyle = palette.receiving;
		context.beginPath();
		context.moveTo(first[0] * width, lane.height);
		for (const [x, y] of lane.level) context.lineTo(x * width, y);
		context.lineTo(last[0] * width, lane.height);
		context.closePath();
		context.fill();
	}

	for (const shape of lane.shapes) {
		const paint = PAINT[shape.style];
		const dimmed = focus !== undefined && shape.style !== "track" && shape.group !== focus;
		const color = palette[paint.color];
		const x = shape.x0 * width;
		const markWidth = Math.max(MIN_WIDTH, (shape.x1 - shape.x0) * width - GAP);

		context.globalAlpha = paint.alpha * (dimmed ? 0.2 : 1);
		if (paint.fill === "outline") {
			context.strokeStyle = color;
			context.lineWidth = 1;
			context.strokeRect(
				x + 0.5,
				shape.y + 0.5,
				Math.max(1, markWidth - 1),
				Math.max(1, shape.height - 1),
			);
		} else {
			context.fillStyle = paint.fill === "hatch" ? hatch(context, color) : color;
			context.fillRect(x, shape.y, markWidth, shape.height);
		}
	}
	context.globalAlpha = 1;
}

const hatches = new Map<string, CanvasPattern>();

// A diagonal-stripe fill in `color`, made once per colour.
function hatch(context: CanvasRenderingContext2D, color: string): CanvasPattern | string {
	const cached = hatches.get(color);
	if (cached) return cached;

	const tile = document.createElement("canvas");
	tile.width = 6;
	tile.height = 6;
	const pen = tile.getContext("2d");
	if (pen === null) return color;
	pen.strokeStyle = color;
	pen.lineWidth = 2;
	pen.beginPath();
	for (const offset of [-6, 0, 6]) {
		pen.moveTo(offset, 6);
		pen.lineTo(offset + 6, 0);
	}
	pen.stroke();

	const pattern = context.createPattern(tile, "repeat");
	if (pattern === null) return color;
	hatches.set(color, pattern);
	return pattern;
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

function formatBitrate(bitsPerSecond: number): string {
	if (bitsPerSecond >= 1_000_000) return `${(bitsPerSecond / 1_000_000).toFixed(2)} Mbps`;
	return `${Math.round(bitsPerSecond / 1000)} kbps`;
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
