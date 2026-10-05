// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals, assertStrictEquals } from "@std/assert";
import {
	describe,
	EventLog,
	formatLog,
	type GroupProblem,
	health,
	incidents,
	type LogEntry,
	type LogEvent,
	severity,
} from "./log.ts";

function skipped(group: number, track = "audio", problem: GroupProblem = "skipped"): LogEvent {
	return { kind: "group", track, problem, first: group, last: group, count: 1 };
}

// Entries one second apart from `at`, so none of them merge.
function entries(at: number, ...events: LogEvent[]): LogEntry[] {
	return events.map((event, i) => ({ at: at + i * 2000, until: at + i * 2000, event }));
}

Deno.test("keeps events in the order they happened", () => {
	const log = new EventLog();

	log.add(0, { kind: "started" });
	log.add(5000, { kind: "ranDry", count: 1 });

	assertEquals(log.entries().map((e) => e.event.kind), ["started", "ranDry"]);
});

Deno.test("merges a run of groups that ended the same way", () => {
	const log = new EventLog();

	log.add(0, skipped(7));
	log.add(400, skipped(8));
	log.add(900, skipped(9));

	assertEquals(log.entries(), [{
		at: 0,
		until: 900,
		event: { kind: "group", track: "audio", problem: "skipped", first: 7, last: 9, count: 3 },
	}]);
});

Deno.test("starts a new entry once a run has paused for more than a second", () => {
	const log = new EventLog();

	log.add(0, skipped(7));
	log.add(1001, skipped(8));

	assertStrictEquals(log.entries().length, 2);
});

Deno.test("merges a run that another kind of event interleaves with", () => {
	const log = new EventLog();

	log.add(0, skipped(7));
	log.add(100, { kind: "audio", loss: "gaps", ms: 21 });
	log.add(200, skipped(8));
	log.add(300, { kind: "audio", loss: "gaps", ms: 21 });

	assertEquals(log.entries().map((e) => describe(e.event)), [
		"audio: 2 groups (7 to 8) skipped (playback moved on before it finished arriving)",
		"42 ms of audio missing (it never arrived)",
	]);
});

Deno.test("keeps the tracks and the ways of ending apart", () => {
	const log = new EventLog();

	log.add(0, skipped(7, "audio"));
	log.add(10, skipped(7, "video"));
	log.add(20, skipped(8, "audio", "late"));

	assertStrictEquals(log.entries().length, 3);
});

Deno.test("a merged stop keeps the count and the longest", () => {
	const log = new EventLog();

	log.add(0, { kind: "stall", ms: 80, count: 1 });
	log.add(300, { kind: "stall", ms: 240, count: 1 });
	log.add(600, { kind: "stall", ms: 60, count: 1 });

	assertEquals(log.entries().map((e) => e.event), [{ kind: "stall", ms: 240, count: 3 }]);
});

Deno.test("forgets the oldest entries past five hundred", () => {
	const log = new EventLog();

	for (let i = 0; i < 502; i++) log.add(i * 2000, { kind: "ranDry", count: 1 });

	assertEquals([log.entries().length, log.entries()[0]?.at], [500, 4000]);
});

Deno.test("describes each event as a sentence", () => {
	const cases: readonly { event: LogEvent; want: string }[] = [
		{ event: { kind: "started" }, want: "Playback started" },
		{
			event: { kind: "delay", from: undefined, to: 104.4 },
			want: "Playback delay set to 104 ms",
		},
		{
			event: { kind: "delay", from: 104, to: 156 },
			want: "Playback delay raised from 104 ms to 156 ms",
		},
		{
			event: skipped(7),
			want: "audio: group 7 skipped (playback moved on before it finished arriving)",
		},
		{ event: { kind: "ranDry", count: 1 }, want: "Audio buffer ran dry" },
		{ event: { kind: "ranDry", count: 3 }, want: "Audio buffer ran dry 3 times" },
		{
			event: { kind: "audio", loss: "starved", ms: 180 },
			want: "180 ms of silence while the audio buffer refilled",
		},
		{ event: { kind: "stall", ms: 247, count: 1 }, want: "The page stopped for 247 ms" },
		{
			event: { kind: "arrival", track: "video", ms: 300, count: 2 },
			want: "video: nothing arrived 2 times, for 300 ms at most",
		},
	];

	for (const c of cases) assertStrictEquals(describe(c.event), c.want);
});

Deno.test("ranks what is heard as a break above what only hints at trouble", () => {
	const cases: readonly { event: LogEvent; want: string }[] = [
		{ event: { kind: "started" }, want: "info" },
		{ event: { kind: "delay", from: 100, to: 150 }, want: "info" },
		{ event: { kind: "ranDry", count: 1 }, want: "bad" },
		{ event: { kind: "audio", loss: "gaps", ms: 21 }, want: "bad" },
		{ event: { kind: "audio", loss: "trimmed", ms: 21 }, want: "warn" },
		{ event: skipped(7), want: "warn" },
		{ event: skipped(7, "audio", "aborted"), want: "bad" },
		{ event: { kind: "stall", ms: 80, count: 1 }, want: "warn" },
	];

	for (const c of cases) assertStrictEquals(severity(c.event), c.want);
});

Deno.test("health is idle until playback has started", () => {
	assertEquals(health([], 1000), { level: "idle", summary: "Not playing" });
});

Deno.test("health is ok when nothing has gone wrong lately", () => {
	const log = entries(0, { kind: "started" }, { kind: "delay", from: undefined, to: 100 });

	assertEquals(health(log, 5000), { level: "ok", summary: "Playing normally" });
});

Deno.test("health names the latest incident of the last ten seconds", () => {
	const log = entries(0, { kind: "started" }, skipped(7), { kind: "ranDry", count: 1 });

	assertEquals(health(log, 9000), {
		level: "bad",
		summary: "Playback is breaking up. 1 group did not play in full: the audio buffer ran dry",
	});
});

Deno.test("health warns when the only trouble is the kind that may not be heard", () => {
	const log = entries(0, { kind: "started" }, { kind: "stall", ms: 80, count: 1 });

	assertEquals(health(log, 5000), {
		level: "warn",
		summary: "Playing, with trouble. The page stopped for up to 80 ms",
	});
});

// Entries at the given times, so the tests can put them near or far apart.
function at(...timed: readonly (readonly [number, LogEvent])[]): LogEntry[] {
	return timed.map(([time, event]) => ({ at: time, until: time, event }));
}

Deno.test("trouble that comes together is one incident, named by its cause and cost", () => {
	const log = at(
		[0, { kind: "started" }],
		[5000, { kind: "arrival", track: "audio", ms: 1031, count: 2 }],
		[5100, { kind: "ranDry", count: 1 }],
		[5100, { kind: "audio", loss: "starved", ms: 590 }],
		[5600, { kind: "audio", loss: "gaps", ms: 918 }],
		[5700, { kind: "delay", from: 180, to: 500 }],
	);

	const items = incidents(log);

	assertEquals(
		items.map((i) => i.kind === "incident" ? i.incident.headline : i.entry.event.kind),
		[
			"started",
			"Nothing arrived for up to 1.0 s: the audio buffer ran dry, 1.5 s of sound lost, delay raised to 500 ms",
		],
	);
});

Deno.test("an incident spans its entries and takes the worst of their levels", () => {
	const log = at(
		[1000, { kind: "stall", ms: 80, count: 1 }],
		[1500, { kind: "ranDry", count: 1 }],
	);

	const [item] = incidents(log);

	assertEquals(
		item?.kind === "incident" && [item.incident.at, item.incident.until, item.incident.level],
		[
			1000,
			1500,
			"bad",
		],
	);
});

Deno.test("the page stopping is the cause when it is among the trouble", () => {
	const log = at(
		[1000, { kind: "arrival", track: "audio", ms: 300, count: 1 }],
		[1100, { kind: "stall", ms: 280, count: 1 }],
	);

	const [item] = incidents(log);

	assertStrictEquals(
		item?.kind === "incident" && item.incident.headline,
		"The page stopped for up to 280 ms",
	);
});

Deno.test("sound lost right after the delay was raised is the price of the raise", () => {
	const log = at(
		[1000, { kind: "delay", from: 100, to: 121 }],
		[1100, { kind: "audio", loss: "starved", ms: 8 }],
	);

	const items = incidents(log);

	assertEquals(items.map((i) => i.kind === "incident" && i.incident.headline), [
		"The playback delay was raised to 121 ms: 8 ms of sound lost",
	]);
});

Deno.test("trouble more than two seconds after the last is a new incident", () => {
	const log = at(
		[1000, { kind: "ranDry", count: 1 }],
		[3001, { kind: "ranDry", count: 1 }],
	);

	assertStrictEquals(incidents(log).length, 2);
});

Deno.test("the delay being set, with no trouble around it, is not an incident", () => {
	const log = at(
		[0, { kind: "started" }],
		[100, { kind: "delay", from: undefined, to: 100 }],
		[5000, { kind: "delay", from: 100, to: 130 }],
	);

	assertEquals(incidents(log).map((i) => i.kind), ["entry", "entry", "entry"]);
});

Deno.test("health recovers ten seconds after the last trouble", () => {
	const log = entries(0, { kind: "started" }, { kind: "ranDry", count: 1 });

	assertStrictEquals(health(log, 12_001).level, "ok");
});

Deno.test("health does not hold a new playback to the trouble of the one before", () => {
	const log = entries(0, { kind: "ranDry", count: 1 }, { kind: "started" });

	assertStrictEquals(health(log, 3000).level, "ok");
});

Deno.test("formats the log as one timed line per entry, oldest first", () => {
	const log = entries(0, { kind: "started" }, { kind: "ranDry", count: 1 });
	// Local midnight, so the expected text does not depend on the time zone.
	const midnight = new Date(2026, 0, 1).getTime();

	const text = formatLog(log, (at) => midnight + at);

	assertStrictEquals(
		text,
		"00:00:00.000  Playback started\n00:00:02.000  Audio buffer ran dry",
	);
});
