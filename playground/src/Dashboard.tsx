import { createEffect, createSignal, Show } from "solid-js";
import { ScenarioPicker } from "./ScenarioPicker.tsx";
import { PathControl } from "./PathControl.tsx";
import { ScenarioView } from "./ScenarioView.tsx";
import { isScenarioId, type ScenarioId, SCENARIOS } from "./scenarios.ts";
import { generateBroadcastId, generateUsername } from "./user/user_name.ts";

// Read ?scenario= and ?path= from the URL (shareable deep links). Falls back to
// "echo" for an unknown/missing scenario.
function readParams(): { scenario: ScenarioId; path: string | null } {
	const params = new URLSearchParams(globalThis.location.search);
	const s = params.get("scenario");
	return { scenario: s && isScenarioId(s) ? s : "echo", path: params.get("path") };
}

// Per-session unique token + friendly name. Generated once so a user keeps the
// same identity across scenario switches, and every default path embeds the
// token — on a shared public relay this prevents two users from colliding on
// the same broadcast path.
const broadcastId = generateBroadcastId();
const friendlyName = generateUsername();

// Each scenario's default path embeds the session's broadcast id so it is
// unique: echo is "/<name>-<id>", ingest is "/<scheme>/<id>". The user can still
// edit/share the path (Copy link), and a deep-linked ?path= overrides this.
function defaultPathFor(scenario: ScenarioId): string {
	switch (scenario) {
		case "echo":
			return `/${friendlyName}-${broadcastId}`;
		case "rtmp":
			return `/rtmp/${broadcastId}`;
		case "rtsp":
			return `/rtsp/${broadcastId}`;
		case "camera":
			return `/live/camera`;
		// Fixed for the same reason as camera: an out-of-band process has to
		// find this path. `qumo hls` is configured with it before the page
		// exists, so a per-session id would mean reconfiguring the egress after
		// every reload — and until you did, it would sit retrying a broadcast
		// nobody publishes to. Two people on one relay would collide here, but a
		// single egress serves a single path anyway, so this scenario is
		// one-at-a-time regardless; ?path= still overrides.
		case "hls":
			return `/hls/live`;
	}
}

export function Dashboard() {
	const initial = readParams();
	const [scenario, setScenario] = createSignal<ScenarioId>(initial.scenario);
	const [path, setPath] = createSignal<string>(initial.path ?? defaultPathFor(initial.scenario));

	// Keep the URL in sync so the current scenario+path is shareable as-is.
	// What else the address carries stays: ?transport= is read when a
	// scenario is entered (transport.ts), so it has to outlive this.
	createEffect(() => {
		const params = new URLSearchParams(globalThis.location.search);
		params.set("scenario", scenario());
		params.set("path", path());
		globalThis.history.replaceState(null, "", `?${params.toString()}`);
	});

	// Switching scenario resets the path to that scenario's default (each
	// embeds the per-session broadcast id). User edits within a scenario are
	// preserved until they switch.
	const selectScenario = (id: ScenarioId) => {
		if (id === scenario()) return;
		setScenario(id);
		setPath(defaultPathFor(id));
	};

	return (
		<div class="dashboard">
			<div class="top-controls">
				<ScenarioPicker scenario={scenario()} onPick={selectScenario} />
				<PathControl scenario={scenario()} path={path} setPath={setPath} />
			</div>

			<p class="scenario-desc">{SCENARIOS[scenario()].description}</p>

			{
				/* Keyed remount: each scenario is a different WebTransport origin, so the
			    session/connection-status/boards rebuild on switch. */
			}
			<Show when={scenario()} keyed>
				<ScenarioView scenario={scenario()} path={path} />
			</Show>
		</div>
	);
}
