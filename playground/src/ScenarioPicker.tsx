import { For } from "solid-js";
import { SCENARIO_ORDER, type ScenarioId, SCENARIOS } from "./scenarios.ts";

// Segmented tab control for picking the active demo scenario.
export function ScenarioPicker(props: {
	scenario: ScenarioId;
	onPick: (id: ScenarioId) => void;
}) {
	return (
		<div class="segmented" role="tablist" aria-label="Demo scenario">
			<For each={SCENARIO_ORDER}>
				{(id) => (
					<button
						type="button"
						role="tab"
						aria-selected={props.scenario === id}
						class="segmented-btn"
						classList={{ active: props.scenario === id }}
						onClick={() => props.onPick(id)}
					>
						{SCENARIOS[id].label}
					</button>
				)}
			</For>
		</div>
	);
}
