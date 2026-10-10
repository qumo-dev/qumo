import { type Accessor, createSignal, type Setter, Show } from "solid-js";
import type { ScenarioId } from "./scenarios.ts";

// The single shared broadcast path: editable, copyable, and shareable as a
// ?scenario=&path= deep link. Echo uses it for both publish and subscribe;
// ingest scenarios use it as the subscribe path.
export function PathControl(props: {
	scenario: ScenarioId;
	path: Accessor<string>;
	setPath: Setter<string>;
}) {
	const [copied, setCopied] = createSignal<"path" | "link" | null>(null);
	const flash = (which: "path" | "link") => {
		setCopied(which);
		setTimeout(() => setCopied((c) => (c === which ? null : c)), 1200);
	};

	const copyPath = () => {
		navigator.clipboard?.writeText(props.path()).then(() => flash("path")).catch(() => {});
	};

	const copyLink = () => {
		const url = new URL(globalThis.location.href);
		url.searchParams.set("scenario", props.scenario);
		url.searchParams.set("path", props.path());
		// A transport tried here is not one to hand on: the browser that
		// opens the link takes the one that works for it.
		url.searchParams.delete("transport");
		navigator.clipboard?.writeText(url.toString()).then(() => flash("link")).catch(() => {});
	};

	return (
		<div class="path-control">
			<label for="broadcast-path">Path</label>
			<input
				id="broadcast-path"
				type="text"
				class="path-input-field"
				value={props.path()}
				onInput={(e) => props.setPath(e.currentTarget.value)}
				placeholder="/rtmp/demo"
				spellcheck={false}
			/>
			<button type="button" class="copy-btn" onClick={copyPath} title="Copy path">
				{copied() === "path" ? "Copied" : "Copy path"}
			</button>
			<button type="button" class="copy-btn" onClick={copyLink} title="Copy shareable link">
				{copied() === "link" ? "Copied" : "Copy link"}
			</button>
			<Show when={props.scenario !== "echo"}>
				<span class="path-hint">subscribe-only — push externally (see command below)</span>
			</Show>
		</div>
	);
}
