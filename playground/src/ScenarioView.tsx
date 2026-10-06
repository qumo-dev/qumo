import { type Accessor, createEffect, createSignal, onCleanup, onMount, Show } from "solid-js";
import { connect, DefaultTrackMux } from "@qumo/moq";
import type { Session } from "@qumo/moq";
import { PublishBoard } from "./publish/PublishBoard.tsx";
import { SubscribeBoard } from "./subscribe/SubscribeBoard.tsx";
import { HlsPlayer } from "./HlsPlayer.tsx";
import { type ConnectionState, ConnectionStatus, friendlyConnError } from "./ConnectionStatus.tsx";
import { sanitizeReason } from "./errors.ts";
import { buildTransportOptions, type CertHashProblem } from "./cert.ts";
import { getConfig, relayHost } from "./config.ts";
import { relayUrlFor, type ScenarioId, SCENARIOS } from "./scenarios.ts";
import { PushInstructions } from "./PushInstructions.tsx";
import { CameraPullForm, type PullState } from "./CameraPullForm.tsx";
import { DevtoolsPanel } from "./devtools/DevtoolsPanel.tsx";
import { Recorder } from "./devtools/recorder.ts";
import { watchMainThread } from "./devtools/stall_monitor.ts";

// Owns one WebTransport session for the active scenario. Each scenario is a
// different origin, so the parent <Show> remounts this component (tearing down
// the old session via onCleanup) whenever the scenario changes.
export function ScenarioView(props: {
	scenario: ScenarioId;
	path: Accessor<string>;
}) {
	const scenario = SCENARIOS[props.scenario];
	const ingest = scenario.mode === "subscribe";
	const isCamera = props.scenario === "camera";
	const isHls = props.scenario === "hls";
	const [pullActive, setPullActive] = createSignal(false);

	// Records what the session's tracks do, for the DevTools panel.
	// It lives and dies with this component, like the session itself.
	const recorder = new Recorder();
	onCleanup(watchMainThread((duration) => recorder.mainThreadStalled(duration)));
	const showsSubscriber = () => !isHls && (isCamera ? pullActive() : true);

	const mux = DefaultTrackMux;
	// Where the relay and the ingest origins are reached; unknown until the
	// runtime config has been read.
	const [host, setHost] = createSignal<string>();

	const [connState, setConnState] = createSignal<ConnectionState>("connecting");
	const [connError, setConnError] = createSignal<string | null>(null);
	const [certHashProblem, setCertHashProblem] = createSignal<CertHashProblem | null>(null);

	let dialSession!: (s: Promise<Session>) => void;
	const session: Promise<Session> = new Promise<Promise<Session>>((resolve) => {
		dialSession = resolve;
	}).then((s) => s);

	let certReady = false;
	let cachedTransportOptions:
		| ReturnType<typeof buildTransportOptions>["transportOptions"]
		| undefined;
	let cachedProblem: CertHashProblem | null = null;

	// Shared dial logic — called once the config is resolved AND (for camera)
	// the pull is active. For non-camera scenarios it fires immediately in
	// onMount.
	const doDial = () => {
		const to = host();
		if (!certReady || to === undefined) return;
		setConnState("connecting");
		const connected = connect(relayUrlFor(props.scenario, to), {
			mux,
			transportOptions: cachedTransportOptions!,
		});
		dialSession(connected);
		connected.then(
			(s) => {
				setConnState("connected");
				s.closed.then(
					(info) => {
						setConnError(
							sanitizeReason(info.reason, "Connection closed by the relay."),
						);
						setConnState("closed");
					},
					(e) => {
						setConnError(
							sanitizeReason(
								e instanceof Error ? e.message : String(e),
								"Connection failed",
							),
						);
						setConnState("failed");
					},
				);
			},
			(e) => {
				setConnError(friendlyConnError(e, cachedProblem));
				setConnState("failed");
			},
		);
	};

	onMount(async () => {
		const cfg = await getConfig();
		const { transportOptions, problem } = buildTransportOptions(cfg.certHash);
		cachedTransportOptions = transportOptions;
		cachedProblem = problem;
		certReady = true;
		setCertHashProblem(problem);
		setHost(relayHost(cfg));

		// Non-camera scenarios connect immediately. Camera waits for pullActive.
		if (!isCamera) {
			doDial();
		}
	});

	// Camera: dial when the pull becomes active.
	createEffect(() => {
		if (isCamera && pullActive() && certReady) {
			doDial();
		}
	});

	onCleanup(() => {
		session.then((s) => s.close().catch(() => {})).catch(() => {});
	});

	return (
		<>
			<Show when={!isCamera || pullActive()}>
				<ConnectionStatus
					state={connState()}
					error={connError()}
					certHashProblem={certHashProblem()}
				/>
			</Show>

			<Show when={isCamera}>
				<CameraPullForm
					path={props.path}
					onStateChange={(s: PullState) => setPullActive(s === "active")}
				/>
			</Show>
			<Show when={ingest && !isCamera ? host() : undefined}>
				{(reached) => (
					<PushInstructions
						scenario={props.scenario}
						path={props.path}
						host={reached()}
					/>
				)}
			</Show>

			<div class={ingest ? "boards single" : "boards"}>
				<Show when={!ingest}>
					<PublishBoard
						mux={mux}
						path={props.path}
						recorder={recorder}
					/>
				</Show>
				<Show when={showsSubscriber()}>
					<SubscribeBoard session={session} path={props.path} observer={recorder} />
				</Show>
				<Show when={isHls}>
					<HlsPlayer path={props.path} />
				</Show>
				<Show when={isCamera && !pullActive()}>
					<div class="video-empty">
						<span class="video-empty-icon">📷</span>
						Enter a camera URL and click "Start Pull" to begin streaming.
					</div>
				</Show>
			</div>

			<Show when={showsSubscriber() || !ingest}>
				<DevtoolsPanel recorder={recorder} session={session} />
			</Show>
		</>
	);
}
