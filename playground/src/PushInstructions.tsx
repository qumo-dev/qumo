import { type Accessor, createSignal, Show } from "solid-js";
import { pushCommandFor, pushTargetFor, type ScenarioId } from "./scenarios.ts";

// Copy-pasteable ffmpeg push command for ingest scenarios (RTMP/RTSP). The
// target URL embeds the current broadcast path — which is unique per session —
// so the external push and the subscriber always agree on the stream. `host`
// is where the ingest origin is reached, as the runtime config names it.
export function PushInstructions(props: {
	scenario: ScenarioId;
	path: Accessor<string>;
	host: string;
}) {
	const [copied, setCopied] = createSignal(false);

	const target = () => pushTargetFor(props.scenario, props.path(), props.host);
	const cmd = () => pushCommandFor(props.scenario, props.path(), props.host);

	const copy = () => {
		const c = cmd();
		if (!c) return;
		navigator.clipboard?.writeText(c).then(() => {
			setCopied(true);
			setTimeout(() => setCopied(false), 1200);
		}).catch(() => {});
	};

	return (
		<Show when={target()}>
			<div class="push-instructions">
				<div class="push-instructions-head">
					<span>
						Push a stream to <code>{target()}</code>, then start subscribing:
					</span>
					<button type="button" class="copy-btn" onClick={copy}>
						{copied() ? "Copied" : "Copy"}
					</button>
				</div>
				<pre>
					<code>{cmd()}</code>
				</pre>
			</div>
		</Show>
	);
}
