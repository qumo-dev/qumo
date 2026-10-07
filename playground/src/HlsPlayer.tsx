import { createEffect, createSignal, onCleanup, Show } from "solid-js";
import { StatsOverlay } from "./components/StatsOverlay.tsx";
import { hlsBaseUrl } from "./config.ts";
import type { Accessor } from "solid-js";
import Hls from "hls.js";
import { createMediaLogger, MediaTags } from "@okdaichi/media-log";

// Tagged logging, matching the publisher side, so a playback failure reads in
// the same console stream as the encode that produced it.
const log = createMediaLogger(MediaTags.decoder);

// How often to re-check whether the egress is serving the playlist yet. The
// interval backs off because the browser logs every failed fetch itself, and a
// fixed one-second poll buries the rest of the console while a stream that has
// not started yet is waited on.
const PROBE_INTERVAL_MS = 1000;
const PROBE_INTERVAL_MAX_MS = 5000;
const PROBE_BACKOFF = 1.5;

// How often the live latency readout is recomputed, matching the publisher's
// one-second stats window so the two panels update in step.
const LATENCY_SAMPLE_MS = 1000;

// liveEdge names the newest segment a playlist lists, or undefined when it lists
// none — a lifetime that has closed and whose replacement has produced nothing
// yet, which is the state between sessions.
//
// The edge is returned rather than a verdict because no single reading of a
// playlist can tell a live stream from one that just stopped; see the probe,
// which compares two.
function liveEdge(playlist: string): string | undefined {
	const segments = playlist
		.split("\n")
		.map((line) => line.trim())
		.filter((line) => line !== "" && !line.startsWith("#"));
	return segments[segments.length - 1];
}

// formatLatency renders a millisecond latency the way a viewer reads it:
// sub-second in ms, beyond that in seconds, where a tenth of a second is
// already below what the number's own sampling can resolve.
function formatLatency(ms: number | undefined): string {
	if (ms === undefined) return "—";
	if (ms < 1000) return `${Math.round(ms)} ms`;
	return `${(ms / 1000).toFixed(1)} s`;
}

// HlsPlayer plays the HLS egress for a track. The egress (`qumo hls`) is a
// separate process that subscribes to the relay and serves HLS. It is taken
// to be on the host the page was opened at, at port 8081 (which must differ
// from the playground's own web UI port); VITE_HLS_URL replaces that.
export function HlsPlayer(props: { path: Accessor<string> }) {
	const base = hlsBaseUrl(globalThis.location, import.meta.env.VITE_HLS_URL);
	// props.path() already starts with "/" (e.g. "/hls/<id>"); avoid a double
	// slash when joining it to base.
	const playlistUrl = () => `${base}${props.path()}/playlist.m3u8`;

	let videoEle: HTMLVideoElement | undefined;
	let hls: Hls | undefined;
	const [status, setStatus] = createSignal<string>("waiting");
	// The egress is a separate process and only serves once the publisher is
	// live, so the playlist genuinely does not exist for the first few seconds.
	// Gate attachment on it being there: hls.js is only given a URL that
	// already resolves, instead of burning its retry budget on a 404 and
	// filling the console with connection errors.
	const [ready, setReady] = createSignal(false);
	// End-to-end latency in milliseconds, or undefined when it cannot be
	// determined yet. Derived from #EXT-X-PROGRAM-DATE-TIME: the egress stamps
	// each group with the wall-clock time it arrived from the relay, and the
	// ledger renders that into the playlist, so the date of the frame on screen
	// minus now is the delay from ingest to display — packaging, ledger write,
	// playlist pickup, and player buffering together.
	const [latencyMs, setLatencyMs] = createSignal<number | undefined>(undefined);
	let latencyTimer: number | undefined;

	// Poll the playlist until it is live. Cleared on cleanup so a scenario switch
	// does not leave a timer running.
	//
	// A playlist that merely answers is not enough. The ledger keeps every group
	// and the egress goes on serving the last window of them after a publisher
	// stops, so a page opened after a stream ended finds a perfectly valid
	// manifest of media minutes old and plays it as though it were live. That is
	// the difference between "the endpoint is up" and "there is something to
	// watch", and only the second is worth attaching to.
	// The newest segment seen on the previous poll, so the next one can tell
	// whether the playlist moved.
	let seenEdge: string | undefined;

	let probeTimer: number | undefined;
	// A generation token so a probe retired by reset() stops itself instead of
	// rescheduling. Without it, a probe whose fetch was in flight when reset()
	// cleared probeTimer resolves after the clear, sets a fresh probeTimer, and
	// keeps polling the old URL — racing the new probe and never stopping.
	let probeGen = 0;
	const probe = async (url: string, delay = PROBE_INTERVAL_MS) => {
		const gen = probeGen;
		try {
			const resp = await fetch(url, { cache: "no-store" });
			if (gen !== probeGen) return;
			if (resp.ok) {
				const edge = liveEdge(await resp.text());
				if (gen !== probeGen) return;
				// Attach only once the playlist has actually moved between two
				// polls. A recent newest segment is not enough: a publisher that
				// stopped a moment ago leaves one just as recent as a publisher
				// still running, and the egress needs a few seconds of silence to
				// notice the difference. Reloading the page lands squarely in that
				// window — the old session is over, its last segment is seconds
				// old, and attaching plays it. Only a playlist that grows proves
				// media is still arriving now.
				if (edge !== undefined && seenEdge !== undefined && edge !== seenEdge) {
					setReady(true);
					return;
				}
				seenEdge = edge;
			}
		} catch {
			if (gen !== probeGen) return;
			// reason: the egress not being up yet is the expected case here; it
			// is reported through the waiting state, not as an error. Whatever
			// edge was seen is void: a server that cannot be reached is not
			// advancing.
			seenEdge = undefined;
		}
		// Back off only while there is nothing to watch. Once a live-looking
		// playlist is in hand the wait is for it to move, which is a second
		// away — backing off there would just delay playback.
		const next = seenEdge !== undefined
			? PROBE_INTERVAL_MS
			: Math.min(delay * PROBE_BACKOFF, PROBE_INTERVAL_MAX_MS);
		// Re-check before scheduling, and guard the closure: reset() may have
		// retired this generation while the fetch was resolving.
		if (gen !== probeGen) return;
		probeTimer = setTimeout(() => {
			if (gen !== probeGen) return;
			void probe(url, next);
		}, delay);
	};

	// Tear the player down and go back to waiting. Used when the stream ends and
	// when the path changes, so a restarted publisher is picked up rather than
	// leaving a dead player showing a stale error.
	const reset = () => {
		probeGen++;
		if (probeTimer !== undefined) clearTimeout(probeTimer);
		probeTimer = undefined;
		if (latencyTimer !== undefined) clearInterval(latencyTimer);
		latencyTimer = undefined;
		seenEdge = undefined;
		hls?.destroy();
		hls = undefined;
		setReady(false);
		setLatencyMs(undefined);
		setStatus("waiting");
	};

	const attach = () => {
		if (!videoEle) return;
		const video = videoEle;
		const url = playlistUrl();
		// hls.js first, native only where it cannot run.
		//
		// canPlayType is not the test it looks like: desktop Chrome answers
		// "maybe" for an HLS playlist and then cannot decode one, so asking it
		// first hands the playlist to an element that does nothing with it —
		// silently, because hls.js never exists to report anything. Where MSE
		// is available hls.js is the playback path; native is the fallback for
		// browsers without it, which is iOS Safari.
		if (Hls.isSupported()) {
			// The playlist is known to resolve before this runs, so hls.js needs
			// no special retry budget — its defaults are right for a live edge.
			hls = new Hls({ liveDurationInfinity: true });
			// Listeners before loading: loadSource starts fetching immediately,
			// and anything it reports before a handler exists is lost — which
			// would hide the very error that explains a player doing nothing.
			hls.on(Hls.Events.MANIFEST_PARSED, (_event, data) => {
				setStatus("playing");
				// The codecs come from parsing the init segment, so this is where
				// an unplayable one shows up: a container the ledger served
				// faithfully can still be a codec MSE will not take.
				log.info("hls: manifest parsed", {
					levels: data.levels.length,
					videoCodec: data.levels[0]?.videoCodec,
					audioCodec: data.levels[0]?.audioCodec,
				});
			});
			hls.on(Hls.Events.ERROR, (_event, data) => {
				// hls.js reports why it cannot play through this event and
				// nowhere else, so it is logged rather than only shown as a
				// status string — a silent player is the hardest thing to
				// diagnose from the outside.
				const report = data.fatal ? log.error : log.warn;
				report("hls: error", {
					type: data.type,
					details: data.details,
					fatal: data.fatal,
					reason: data.reason,
					err: data.error?.message,
				});
				if (!data.fatal) {
					setStatus(`recovering (${data.details})`);
					return;
				}
				// A fatal network error means the egress stopped serving — the
				// publisher went away. Go back to waiting so a restarted stream
				// is picked up, instead of sitting on a dead player.
				if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
					reset();
					void probe(playlistUrl());
					return;
				}
				setStatus(`error: ${data.type}`);
			});

			// Listeners are attached, so nothing loading reports can be missed.
			hls.loadSource(url);
			hls.attachMedia(video);

			// playingDate is the wall-clock time of the frame currently on
			// screen, resolved by hls.js from the playlist's
			// #EXT-X-PROGRAM-DATE-TIME. Null until a segment with a date is
			// being played, and while paused it drifts by design — the frame on
			// screen really is that old.
			latencyTimer = setInterval(() => {
				const playingDate = hls?.playingDate;
				setLatencyMs(playingDate ? Date.now() - playingDate.getTime() : undefined);
			}, LATENCY_SAMPLE_MS);
		} else if (video.canPlayType("application/vnd.apple.mpegurl")) {
			video.src = url;
			setStatus("native");
		} else {
			setStatus("unsupported");
		}
	};

	// Restart the probe whenever the target changes: a new broadcast path is a
	// different stream, so any attached player is torn down first.
	createEffect(() => {
		const url = playlistUrl();
		reset();
		void probe(url);
	});

	// Attach once the playlist becomes available.
	createEffect(() => {
		if (ready()) attach();
	});

	onCleanup(reset);

	return (
		<div class="subscribe-board">
			<h2>HLS Player</h2>
			<p class="status-message">
				playlist: <code>{playlistUrl()}</code>
			</p>
			{
				/* The video element stays mounted so its ref is always available
			    when the playlist becomes ready; the waiting modifier just
			    restyles it until the first frame arrives.

			    Playback starts on the viewer's click rather than on attach.
			    hls.js still buffers the first fragment, so the browser paints
			    that frame as soon as it decodes — the panel shows a still from
			    the stream instead of an empty box, and the element is its own
			    poster with no second decode path to manage. */
			}
			<div class={ready() ? "video-preview" : "video-preview video-preview--waiting"}>
				<video ref={videoEle} controls muted playsinline preload="auto" />
				<Show when={latencyMs() !== undefined}>
					<StatsOverlay
						stats={[{ label: "latency", value: formatLatency(latencyMs()) }]}
					/>
				</Show>
			</div>
			<Show when={!ready()}>
				<p class="status-message">Waiting for the stream to start…</p>
			</Show>
			<Show when={ready() && status() !== "playing" && status() !== "native"}>
				<p class="status-message">HLS: {status()}</p>
			</Show>
		</div>
	);
}
