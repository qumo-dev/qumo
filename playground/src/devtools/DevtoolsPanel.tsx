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
import {
	clockTime,
	describe,
	formatLog,
	type Health,
	health,
	type Incident,
	incidents,
	type LogEntry,
	type LogItem,
} from "./log.ts";
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
	type Shape,
	shapeAt,
	type ShapeStyle,
	stallBands,
	trackLane,
	withBands,
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
	/** What has happened, in words, oldest first. */
	readonly log: readonly LogEntry[];
}

// A stretch of time on the recorder's clock.
interface Stretch {
	readonly from: number;
	readonly to: number;
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
		log: [],
	});
	const [session, setSession] = createSignal<SessionReading>();
	const [span, setSpan] = createSignal<Span>(60_000);
	const [paused, setPaused] = createSignal(false);
	const [focus, setFocus] = createSignal<Focus>();
	// The log item the pointer is on, so its time can be found on the timeline.
	const [marked, setMarked] = createSignal<Stretch>();

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
				log: props.recorder.log(),
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
					<StatusLine health={health(reading().log, reading().now)} />
					<KeyFigures
						media={mediaRate(reading())}
						timing={reading().timing}
						audio={reading().audio.at(-1)}
					/>
					<details class="devtools-more">
						<summary>More figures</summary>
						<SessionRow reading={session()} stalls={reading().stalls} />
						<Show when={reading().audio.at(-1)}>
							{(audio) => (
								<AudioBufferRow
									audio={audio()}
									history={reading().audio}
									delays={reading().delays}
								/>
							)}
						</Show>
					</details>

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
							marked={marked()}
						/>
					</Show>
					<EventList log={reading().log} onMark={setMarked} />
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
	stalls: readonly StallRecord[];
}) {
	const longest = () => props.stalls.reduce((most, s) => Math.max(most, s.duration), 0);

	return (
		<dl class="devtools-session">
			<div title="How often, and for how long at most, the page's main thread stopped in the last minute. Media passes through it, so a stop longer than the audio buffer is a gap in the sound.">
				<dt>Main thread stops</dt>
				<dd>
					{props.stalls.length === 0
						? "none"
						: `${props.stalls.length}, longest ${Math.round(longest())} ms`}
				</dd>
			</div>
			<Show when={(props.reading?.rtt ?? 0) > 0}>
				<div title="The connection's round-trip time, as the browser measures it.">
					<dt>RTT</dt>
					<dd>{Math.round(props.reading?.rtt ?? 0)} ms</dd>
				</div>
			</Show>
			<Show when={(props.reading?.bytesReceived ?? 0) > 0}>
				<div title="Everything received on the connection since it opened, and the rate over the last second. More than the media: it includes the protocol's own messages.">
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
				<div title="Everything sent on the connection since it opened, and the rate over the last second.">
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
				<div title="How fast the browser thinks this connection can send.">
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
			<div title="Times the audio buffer emptied since the page loaded, and the silence played while it refilled. Each one is a break in the sound.">
				<dt>Ran dry</dt>
				<dd>
					{`${props.audio.underruns}× (${Math.round(props.audio.starved)} ms silent)`}
				</dd>
			</div>
			<div title="Silence played, since the page loaded, where a frame of audio never arrived.">
				<dt>Missing</dt>
				<dd>{Math.round(props.audio.gaps)} ms</dd>
			</div>
			<div title="Audio, since the page loaded, that arrived after its time had already been played.">
				<dt>Too late</dt>
				<dd>{Math.round(props.audio.late)} ms</dd>
			</div>
			<div title="Audio dropped, since the page loaded, because more arrived at once than the buffer holds. A backlog passed over as playback starts is not counted.">
				<dt>Overflowed</dt>
				<dd>{Math.round(props.audio.overflowed)} ms</dd>
			</div>
			<div title="Audio skipped, since the page loaded, to bring the delay back down after the buffer had held more than it needed.">
				<dt>Trimmed</dt>
				<dd>{Math.round(props.audio.trimmed)} ms</dd>
			</div>
			<div title="Stretches of 120 ms or more, in the last minute, in which no audio came off the connection.">
				<dt>Not arriving</dt>
				<dd>{delayed("arrival")}</dd>
			</div>
			<div title="Times, in the last minute, audio that had arrived waited 20 ms or more in the player for an earlier group.">
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

// How playback is going, in one line. The rest of the panel is the evidence.
function StatusLine(props: { health: Health }) {
	return (
		<p class={`devtools-status is-${props.health.level}`} role="status">
			<span class="devtools-status-mark" aria-hidden="true" />
			{props.health.summary}
		</p>
	);
}

// The few figures that say whether playback is healthy. Everything else is
// under "More figures".
function KeyFigures(props: {
	media: number;
	timing: PlaybackTimingRecord | undefined;
	audio: AudioBufferRecord | undefined;
}) {
	return (
		<dl class="devtools-session">
			<div title="Media arriving on the played tracks, over the last second.">
				<dt>Media received</dt>
				<dd>{formatBitrate(props.media)}</dd>
			</div>
			<Show when={props.timing}>
				{(timing) => (
					<>
						<div title="How far playback trails the live edge. About 100 to 200 ms is usual. It grows to cover the arrival jitter, and when the audio buffer runs dry; it stops at 500 ms.">
							<dt>Playback delay</dt>
							<dd>{Math.round(timing().delay)} ms</dd>
						</div>
						<div title="How unevenly media arrives: how late it has recently come against its fastest arrival. The delay has to cover this.">
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
			<Show when={props.audio}>
				{(audio) => (
					<div title="Audio waiting to be played, against the delay it aims to hold. It should stay near the aim; at zero the sound breaks.">
						<dt>Audio buffer</dt>
						<dd>
							{`${Math.round(audio().buffered)} ms of ${
								Math.round(audio().latency)
							} ms`}
							{audio().stalled ? " (refilling)" : ""}
						</dd>
					</div>
				)}
			</Show>
		</dl>
	);
}

// How many items the list shows. Copy takes every entry.
const LOG_SHOWN = 100;
// How long the Copy button says it has copied.
const COPIED_MS = 1500;

// What happened, in words. Trouble that came together is one incident, headed
// by its likely cause and what it cost, so it can be read after the fact; its
// entries are underneath. Pointing at an item marks its time on the timeline.
function EventList(props: {
	log: readonly LogEntry[];
	onMark: (stretch: Stretch | undefined) => void;
}) {
	const [copied, setCopied] = createSignal(false);
	// Which incidents are open, by when they began: the list is redrawn with
	// every reading, and an incident should stay as the reader left it.
	const [opened, setOpened] = createSignal<ReadonlySet<number>>(new Set());
	// Newest first, so what just happened is at the top without scrolling.
	const shown = createMemo(() => incidents(props.log).slice(-LOG_SHOWN).reverse());

	const copy = () => {
		// The clipboard is absent outside a secure context, such as plain http
		// on a LAN address.
		navigator.clipboard?.writeText(formatLog(props.log, wallClock)).then(() => {
			setCopied(true);
			setTimeout(() => setCopied(false), COPIED_MS);
			// reason: without clipboard access the log can still be selected and copied by hand.
		}, () => {});
	};

	const setOpen = (at: number, open: boolean) => {
		if (opened().has(at) === open) return;
		const next = new Set(opened());
		if (open) next.add(at);
		else next.delete(at);
		setOpened(next);
	};

	return (
		<section class="devtools-log">
			<div class="devtools-controls">
				<h3 class="devtools-heading">Log</h3>
				<button
					type="button"
					class="copy-btn"
					disabled={props.log.length === 0}
					onClick={copy}
				>
					{copied() ? "Copied" : "Copy"}
				</button>
			</div>
			<Show
				when={shown().length > 0}
				fallback={<p class="devtools-empty">Nothing has happened yet.</p>}
			>
				<ol class="devtools-events" reversed onMouseLeave={() => props.onMark(undefined)}>
					<Index each={shown()}>
						{(item) => (
							<Show
								when={incidentOf(item())}
								fallback={
									<li
										class="is-info"
										onMouseEnter={() => props.onMark(stretchOf(item()))}
									>
										<EventLine entry={entryOf(item())} />
									</li>
								}
							>
								{(incident) => (
									<li
										class={`is-${incident().level}`}
										onMouseEnter={() => props.onMark(stretchOf(item()))}
									>
										<details
											open={opened().has(incident().at)}
											onToggle={(event) =>
												setOpen(incident().at, event.currentTarget.open)}
										>
											<summary>
												<time>{clockTime(wallClock(incident().at))}</time>
												<span>{incident().headline}</span>
											</summary>
											<ol>
												<Index each={incident().entries}>
													{(entry) => (
														<li>
															<EventLine entry={entry()} />
														</li>
													)}
												</Index>
											</ol>
										</details>
									</li>
								)}
							</Show>
						)}
					</Index>
				</ol>
			</Show>
		</section>
	);
}

function EventLine(props: { entry: LogEntry | undefined }) {
	return (
		<Show when={props.entry}>
			{(entry) => (
				<>
					<time>{clockTime(wallClock(entry().at))}</time>
					<span>{describe(entry().event)}</span>
				</>
			)}
		</Show>
	);
}

function incidentOf(item: LogItem): Incident | undefined {
	return item.kind === "incident" ? item.incident : undefined;
}

function entryOf(item: LogItem): LogEntry | undefined {
	return item.kind === "entry" ? item.entry : undefined;
}

function stretchOf(item: LogItem): Stretch {
	return item.kind === "incident"
		? { from: item.incident.at, to: item.incident.until }
		: { from: item.entry.at, to: item.entry.until };
}

// The recorder's times are on the page's monotonic clock; this is the same
// moment in milliseconds since the epoch.
function wallClock(at: number): number {
	return Date.now() - (performance.now() - at);
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
						<th scope="col" title="Media received on the track over the last second.">
							Bitrate
						</th>
						<th scope="col" title="Frames received on the track over the last second.">
							Frames/s
						</th>
						<th
							scope="col"
							title="The highest group number seen. A group is a run of frames that can be played without the ones before it: for video, a keyframe and what follows."
						>
							Latest group
						</th>
						<th scope="col" title="Groups received in full, since the page loaded.">
							Complete
						</th>
						<th
							scope="col"
							title="Groups the player moved on from before they had finished arriving."
						>
							Skipped
						</th>
						<th scope="col" title="Groups the sender gave up part-way.">Aborted</th>
						<th
							scope="col"
							title="Groups that arrived after playback had already passed them."
						>
							Late
						</th>
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
	marked: Stretch | undefined;
}) {
	// What is picked out across every lane: the page's stops, during which
	// nothing in any lane moves, and the log item being pointed at.
	const bands = createMemo(() => {
		const stops = stallBands(props.reading.stalls);
		const mark = props.marked;
		if (mark === undefined) return stops;
		return [...stops, {
			from: mark.from - MARK_PAD_MS,
			to: mark.to + MARK_PAD_MS,
			style: "marker" as const,
		}];
	});
	const banded = (lane: Lane) => withBands(lane, bands(), props.reading.now, props.span);
	const axisTime = (ago: number) =>
		clockTime(wallClock(props.reading.now - ago)).slice(0, CLOCK_SECONDS);

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
						lane={banded(trackLane(
							track().groups,
							track().renders,
							props.reading.now,
							props.span,
						))}
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
					name="audio buffer"
					label={`Audio buffer: its level, and where sound was lost, over the last ${
						props.span / 1000
					} seconds`}
					lane={banded(audioLane(
						props.reading.audio,
						props.reading.delays,
						props.reading.now,
						props.span,
					))}
					palette={palette()}
					focus={undefined}
				/>
			</Show>

			<div class="devtools-lane devtools-axis" aria-hidden="true">
				<span />
				<div>
					<span>{axisTime(props.span)}</span>
					<span>{axisTime(props.span / 2)}</span>
					<span>{axisTime(0)} (now)</span>
				</div>
			</div>

			<dl class="devtools-legend">
				<For each={LEGEND}>
					{(row) => (
						<div>
							<dt title={row.about}>{row.lane}</dt>
							<dd>
								<For each={row.marks}>
									{([style, label, meaning]) => (
										<span class="devtools-legend-mark" title={meaning}>
											<span class={`devtools-swatch is-${style}`} />
											{label}
										</span>
									)}
								</For>
							</dd>
						</div>
					)}
				</For>
			</dl>
			<p class="devtools-hint">
				Point at a mark on the timeline for what it is and when it happened.
			</p>
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
	// What the pointer is on, and where along the lane, in pixels.
	const [tip, setTip] = createSignal<{ shape: Shape; x: number }>();
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
		const x = Math.min(Math.max(event.clientX - box.left, TIP_REACH), box.width - TIP_REACH);
		setTip(hit && { shape: hit, x });
		if (hit?.group === pointed) return;
		pointed = hit?.group;
		props.onFocus?.(pointed);
	};

	const leave = () => {
		setTip(undefined);
		if (pointed === undefined) return;
		pointed = undefined;
		props.onFocus?.(undefined);
	};

	return (
		<div class="devtools-lane">
			<span class="devtools-lane-name">{props.name}</span>
			<div class="devtools-plot-box">
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
				<Show when={tip()}>
					{(shown) => (
						<div class="devtools-tip" role="tooltip" style={{ left: `${shown().x}px` }}>
							<Show when={shown().shape.at}>
								{(at) => <time>{clockSpan(at()[0], at()[1])}</time>}
							</Show>
							<For each={(shown().shape.title ?? "").split("\n")}>
								{(line) => <span>{line}</span>}
							</For>
						</div>
					)}
				</Show>
			</div>
		</div>
	);
}

// The log's times are shown to the millisecond; the axis to the second.
const CLOCK_SECONDS = "HH:MM:SS".length;
// A log item's stretch is widened by this on the timeline, so a single moment
// is a band wide enough to see.
const MARK_PAD_MS = 250;

// Half the width the pointer's tip may take, in pixels: it is kept this far
// from either end of the lane so it does not hang over the edge.
const TIP_REACH = 150;

// A stretch on the recorder's clock as clock times; one time if it is a moment.
function clockSpan(from: number, to: number): string {
	const start = clockTime(wallClock(from));
	const end = clockTime(wallClock(to));
	return start === end ? start : `${start} to ${end}`;
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

// How each kind of mark is painted. No two marks that share a row look alike,
// and they differ in more than colour (solid, hatched, outlined; a block, a
// tick, a strip), so they can be told apart without telling the colours apart.
// Across rows a colour keeps its meaning: grey is as expected, blue is in
// progress, green was played, amber is the player or the page getting in the
// way, red is something lost.
const PAINT: Record<
	ShapeStyle,
	{ color: keyof Palette; alpha: number; fill: "solid" | "hatch" | "outline" }
> = {
	complete: { color: "complete", alpha: 0.55, fill: "solid" },
	receiving: { color: "receiving", alpha: 1, fill: "solid" },
	stopped: { color: "stopped", alpha: 0.7, fill: "hatch" },
	skipped: { color: "skipped", alpha: 1, fill: "hatch" },
	aborted: { color: "aborted", alpha: 1, fill: "solid" },
	late: { color: "late", alpha: 1, fill: "outline" },
	rendered: { color: "rendered", alpha: 1, fill: "solid" },
	unrendered: { color: "skipped", alpha: 1, fill: "solid" },
	glitch: { color: "aborted", alpha: 1, fill: "solid" },
	stall: { color: "skipped", alpha: 0.45, fill: "solid" },
	marker: { color: "receiving", alpha: 0.45, fill: "solid" },
	arrival: { color: "late", alpha: 1, fill: "solid" },
	held: { color: "skipped", alpha: 1, fill: "solid" },
	track: { color: "surface", alpha: 1, fill: "solid" },
};

type LegendMark = readonly [style: string, label: string, meaning: string];

// The legend, a row for each kind of row on the timeline, so a mark is looked
// up where it is seen and only has to differ from the others on its own row.
const LEGEND: readonly {
	readonly lane: string;
	readonly about: string;
	readonly marks: readonly LegendMark[];
}[] = [
	{
		lane: "Arrived",
		about: "The upper row of each track: when each group arrived, and how it ended.",
		marks: [
			["complete", "Received", "A group that arrived in full."],
			["receiving", "Receiving", "A group still arriving."],
			[
				"skipped",
				"Skipped",
				"A group the player moved on from before it had finished arriving.",
			],
			["aborted", "Aborted", "A group the sender gave up part-way."],
			["late", "Late", "A group that arrived after playback had already passed it."],
			["stopped", "Stopped", "A group cut short because playback was stopped."],
		],
	},
	{
		lane: "Played",
		about: "The lower row of each track: when its frames were played.",
		marks: [
			[
				"rendered",
				"Played",
				"When a group's frames were drawn or handed to the audio output.",
			],
			["unrendered", "Not played", "Frames that arrived and were never drawn or played."],
		],
	},
	{
		lane: "audio buffer",
		about: "The audio waiting to be played, and what interrupted it.",
		marks: [
			["level", "Buffer level", "How much audio was waiting to be played."],
			["glitch", "Sound lost", "The audio output lost sound or gave some up here."],
			[
				"arrival",
				"Nothing arriving",
				"No audio came off the connection for 120 ms or more.",
			],
			[
				"held",
				"Waited",
				"Audio that had arrived waited 20 ms or more in the player for an earlier group.",
			],
		],
	},
	{
		lane: "Every row",
		about: "Bands drawn across all the rows.",
		marks: [
			[
				"stall",
				"Page stopped",
				"The page's main thread stopped. All media passes through it, so nothing in any row moved.",
			],
			["marker", "In the log", "The time of the log item the pointer is on."],
		],
	},
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
