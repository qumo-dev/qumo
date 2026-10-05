// What happened during playback, as events a person can read.
//
// The timeline shows the same things as marks; this is the list in words, with
// the time of each, that can be read after the fact or copied into a report.

/** Why a group did not play in full. */
export type GroupProblem = "skipped" | "aborted" | "late";

/** A way the audio output lost sound, or gave some up. */
export type AudioLoss = "starved" | "gaps" | "late" | "overflowed" | "trimmed";

export type LogEvent =
	| { readonly kind: "started" }
	| { readonly kind: "stopped" }
	/** The playback delay was set, or changed from `from`. Milliseconds. */
	| { readonly kind: "delay"; readonly from: number | undefined; readonly to: number }
	/** `count` groups of `track`, from sequence `first` to `last`, ended the same way. */
	| {
		readonly kind: "group";
		readonly track: string;
		readonly problem: GroupProblem;
		readonly first: number;
		readonly last: number;
		readonly count: number;
	}
	/** The audio buffer emptied, `count` times. */
	| { readonly kind: "ranDry"; readonly count: number }
	/** `ms` milliseconds of sound were lost or given up. */
	| { readonly kind: "audio"; readonly loss: AudioLoss; readonly ms: number }
	/** The page's main thread stopped `count` times, for `ms` milliseconds at most. */
	| { readonly kind: "stall"; readonly ms: number; readonly count: number }
	/** Nothing of `track` arrived for `ms` milliseconds at most, `count` times. */
	| {
		readonly kind: "arrival";
		readonly track: string;
		readonly ms: number;
		readonly count: number;
	};

export interface LogEntry {
	/** When it happened (for a merged entry, when it first did), in milliseconds on the recorder's clock. */
	readonly at: number;
	/** When it last happened, for an entry that merges several. */
	readonly until: number;
	readonly event: LogEvent;
}

// Events of one kind that follow each other this closely are one entry: a
// burst of skipped groups reads as "12 groups skipped", not twelve lines.
const MERGE_MS = 1000;
const MAX_ENTRIES = 500;

/** The events of a session in order, with runs of the same event merged. */
export class EventLog {
	readonly #entries: LogEntry[] = [];

	add(at: number, event: LogEvent): void {
		// Different kinds of trouble come interleaved, so the run this event
		// continues need not be the last entry.
		for (let i = this.#entries.length - 1; i >= 0; i--) {
			const entry = this.#entries[i];
			if (entry === undefined || at - entry.until > MERGE_MS) break;
			// A run does not carry on across a start or a stop: what follows
			// belongs to the next stretch of playback.
			if (entry.event.kind === "started" || entry.event.kind === "stopped") break;
			const merged = merge(entry.event, event);
			if (merged === undefined) continue;
			this.#entries[i] = { at: entry.at, until: at, event: merged };
			return;
		}
		this.#entries.push({ at, until: at, event });
		if (this.#entries.length > MAX_ENTRIES) this.#entries.shift();
	}

	/** The entries, oldest first. */
	entries(): LogEntry[] {
		return this.#entries.slice();
	}
}

// The one event that says both, when `next` is more of what `previous` was.
function merge(previous: LogEvent, next: LogEvent): LogEvent | undefined {
	if (previous.kind === "group" && next.kind === "group") {
		if (previous.track !== next.track || previous.problem !== next.problem) return undefined;
		return {
			...previous,
			first: Math.min(previous.first, next.first),
			last: Math.max(previous.last, next.last),
			count: previous.count + next.count,
		};
	}
	if (previous.kind === "ranDry" && next.kind === "ranDry") {
		return { kind: "ranDry", count: previous.count + next.count };
	}
	if (previous.kind === "audio" && next.kind === "audio") {
		if (previous.loss !== next.loss) return undefined;
		return { ...previous, ms: previous.ms + next.ms };
	}
	if (previous.kind === "stall" && next.kind === "stall") {
		return {
			kind: "stall",
			ms: Math.max(previous.ms, next.ms),
			count: previous.count + next.count,
		};
	}
	if (previous.kind === "arrival" && next.kind === "arrival") {
		if (previous.track !== next.track) return undefined;
		return {
			...previous,
			ms: Math.max(previous.ms, next.ms),
			count: previous.count + next.count,
		};
	}
	return undefined;
}

/** How much an event matters to someone watching or listening. */
export type Severity = "info" | "warn" | "bad";

/** `bad` is heard or seen as a break; `warn` is a sign of trouble that may not be. */
export function severity(event: LogEvent): Severity {
	switch (event.kind) {
		case "started":
		case "stopped":
		case "delay":
			return "info";
		case "ranDry":
			return "bad";
		case "audio":
			return event.loss === "trimmed" || event.loss === "late" ? "warn" : "bad";
		case "group":
			return event.problem === "aborted" ? "bad" : "warn";
		case "stall":
		case "arrival":
			return "warn";
		default: {
			const unknown: never = event;
			return unknown;
		}
	}
}

const GROUP_PROBLEMS: Record<GroupProblem, string> = {
	skipped: "skipped (playback moved on before it finished arriving)",
	aborted: "aborted (the sender gave it up)",
	late: "arrived too late (playback had already passed it)",
};

const AUDIO_LOSSES: Record<AudioLoss, (ms: string) => string> = {
	starved: (ms) => `${ms} of silence while the audio buffer refilled`,
	gaps: (ms) => `${ms} of audio missing (it never arrived)`,
	late: (ms) => `${ms} of audio arrived after its time had been played`,
	overflowed: (ms) => `${ms} of audio dropped (more arrived at once than the buffer holds)`,
	trimmed: (ms) => `${ms} of audio skipped to bring the delay back down`,
};

/** The event as a sentence. */
export function describe(event: LogEvent): string {
	const ms = (value: number) => `${Math.round(value)} ms`;
	switch (event.kind) {
		case "started":
			return "Playback started";
		case "stopped":
			return "Playback stopped";
		case "delay":
			return event.from === undefined
				? `Playback delay set to ${ms(event.to)}`
				: `Playback delay raised from ${ms(event.from)} to ${ms(event.to)}`;
		case "group":
			return event.count === 1
				? `${event.track}: group ${event.first} ${GROUP_PROBLEMS[event.problem]}`
				: `${event.track}: ${event.count} groups (${event.first} to ${event.last}) ${
					GROUP_PROBLEMS[event.problem]
				}`;
		case "ranDry":
			return event.count === 1
				? "Audio buffer ran dry"
				: `Audio buffer ran dry ${event.count} times`;
		case "audio":
			return AUDIO_LOSSES[event.loss](ms(event.ms));
		case "stall":
			return event.count === 1
				? `The page stopped for ${ms(event.ms)}`
				: `The page stopped ${event.count} times, for ${ms(event.ms)} at most`;
		case "arrival":
			return event.count === 1
				? `${event.track}: nothing arrived for ${ms(event.ms)}`
				: `${event.track}: nothing arrived ${event.count} times, for ${
					ms(event.ms)
				} at most`;
		default: {
			const unknown: never = event;
			return unknown;
		}
	}
}

/**
 * Trouble that came together: what set it off and what it cost, read as one
 * thing. A stretch with nothing arriving, the buffer running dry after it, and
 * the delay being raised in answer are three entries and one incident.
 */
export interface Incident {
	/** When its first entry began and its last one ended, on the recorder's clock. */
	readonly at: number;
	readonly until: number;
	/** The worst of its entries. */
	readonly level: Exclude<Severity, "info">;
	/** The likely cause and what it cost, in one line. */
	readonly headline: string;
	readonly entries: readonly LogEntry[];
}

/** The log as it is read: incidents, and between them the entries that are no trouble. */
export type LogItem =
	| { readonly kind: "entry"; readonly at: number; readonly entry: LogEntry }
	| { readonly kind: "incident"; readonly at: number; readonly incident: Incident };

// Trouble this close to earlier trouble is the same incident.
const INCIDENT_GAP_MS = 2000;

/**
 * Groups the entries into incidents, oldest first. The delay being raised
 * during an incident belongs to it; other entries that are no trouble stand
 * alone.
 */
export function incidents(entries: readonly LogEntry[]): LogItem[] {
	const items: LogItem[] = [];
	let open: LogEntry[] = [];
	const close = () => {
		const first = open[0];
		if (first !== undefined) {
			items.push({ kind: "incident", at: first.at, incident: incidentOf(open) });
		}
		open = [];
	};

	for (const entry of entries) {
		const last = open.at(-1);
		if (last !== undefined && entry.at - until(open) > INCIDENT_GAP_MS) close();

		const raised = entry.event.kind === "delay" && entry.event.from !== undefined;
		if (severity(entry.event) !== "info" || (raised && open.length > 0)) {
			// Raising the delay holds the sound back while the buffer fills,
			// so a raise just before the trouble is where it began.
			const before = items.at(-1);
			if (
				open.length === 0 && before?.kind === "entry" && isRaise(before.entry) &&
				entry.at - before.entry.until <= INCIDENT_GAP_MS
			) {
				items.pop();
				open.push(before.entry);
			}
			open.push(entry);
		} else {
			items.push({ kind: "entry", at: entry.at, entry });
		}
	}
	close();

	return items.sort((a, b) => a.at - b.at);
}

function isRaise(entry: LogEntry): boolean {
	return entry.event.kind === "delay" && entry.event.from !== undefined;
}

function until(entries: readonly LogEntry[]): number {
	return entries.reduce((latest, e) => Math.max(latest, e.until), -Infinity);
}

function incidentOf(entries: readonly LogEntry[]): Incident {
	const bad = entries.some((e) => severity(e.event) === "bad");
	return {
		at: entries[0]?.at ?? 0,
		until: until(entries),
		level: bad ? "bad" : "warn",
		headline: headline(entries.map((e) => e.event)),
		entries,
	};
}

// The cause is the earliest link in the chain that the events show: the page
// stopping holds everything up, nothing arriving starves the buffer, and a
// group that does not play leaves a hole. What follows from it is the cost.
function headline(events: readonly LogEvent[]): string {
	const span = (ms: number) =>
		ms >= 1000 ? `${(ms / 1000).toFixed(1)} s` : `${Math.round(ms)} ms`;
	let stall = 0;
	let arrival = 0;
	let groups = 0;
	let ranDry = 0;
	let lost = 0;
	let raised: number | undefined;
	for (const event of events) {
		if (event.kind === "stall") stall = Math.max(stall, event.ms);
		else if (event.kind === "arrival") arrival = Math.max(arrival, event.ms);
		else if (event.kind === "group") groups += event.count;
		else if (event.kind === "ranDry") ranDry += event.count;
		else if (event.kind === "audio") lost += event.ms;
		else if (event.kind === "delay") raised = event.to;
	}

	const upstream = stall > 0
		? `The page stopped for up to ${span(stall)}`
		: arrival > 0
		? `Nothing arrived for up to ${span(arrival)}`
		: groups > 0
		? `${groups} ${groups === 1 ? "group" : "groups"} did not play in full`
		: undefined;
	// With nothing upstream to blame, a raise of the delay is itself the
	// cause: it is paid for with a moment of silence. After the buffer has
	// run dry, the raise is the answer to it instead.
	const raiseIsCause = upstream === undefined && raised !== undefined && ranDry === 0;
	const cause = raiseIsCause && raised !== undefined
		? `The playback delay was raised to ${span(raised)}`
		: upstream;

	const cost: string[] = [];
	if (ranDry > 0) cost.push("the audio buffer ran dry");
	if (lost > 0) cost.push(`${span(lost)} of sound lost`);
	if (raised !== undefined && !raiseIsCause) cost.push(`delay raised to ${span(raised)}`);

	if (cause === undefined) {
		const text = cost.join(", ");
		return text.charAt(0).toUpperCase() + text.slice(1);
	}
	return cost.length > 0 ? `${cause}: ${cost.join(", ")}` : cause;
}

export interface Health {
	/** `idle` until playback has started. */
	readonly level: "idle" | "ok" | Exclude<Severity, "info">;
	readonly summary: string;
}

// How far back trouble still counts against "playing normally".
const HEALTH_WINDOW_MS = 10_000;

/**
 * Says in one line how playback is going at `now`: the latest incident of the
 * last few seconds, or that there was none.
 */
export function health(entries: readonly LogEntry[], now: number): Health {
	const started = entries.findLast((e) =>
		e.event.kind === "started" || e.event.kind === "stopped"
	);
	if (started?.event.kind !== "started") return { level: "idle", summary: "Not playing" };

	const latest = incidents(entries.filter((e) => e.at >= started.at)).findLast((item) =>
		item.kind === "incident" && item.incident.until >= now - HEALTH_WINDOW_MS
	);
	if (latest?.kind !== "incident") return { level: "ok", summary: "Playing normally" };

	const { level, headline } = latest.incident;
	return level === "bad"
		? { level, summary: `Playback is breaking up. ${headline}` }
		: { level, summary: `Playing, with trouble. ${headline}` };
}

/**
 * The log as text, one line per entry, oldest first. `wallClock` turns a time
 * on the recorder's clock into milliseconds since the epoch.
 */
export function formatLog(
	entries: readonly LogEntry[],
	wallClock: (at: number) => number,
): string {
	return entries.map((e) => `${clockTime(wallClock(e.at))}  ${describe(e.event)}`).join("\n");
}

/** A moment as local `HH:MM:SS.mmm`. */
export function clockTime(epochMs: number): string {
	const date = new Date(epochMs);
	const two = (value: number) => String(value).padStart(2, "0");
	return `${two(date.getHours())}:${two(date.getMinutes())}:${two(date.getSeconds())}.${
		String(date.getMilliseconds()).padStart(3, "0")
	}`;
}
