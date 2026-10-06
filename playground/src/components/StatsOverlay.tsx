import { Index, type JSX } from "solid-js";

/** One figure on the overlay: a short label and its value. */
export interface Stat {
	label: string;
	value: string | number;
}

// The figures laid over a video: resolution, frame rate, bitrate and the
// like. It is drawn over the corner of whatever it is placed in, which has to
// be positioned (a `.video-preview` is).
export function StatsOverlay(props: { stats: readonly Stat[] }): JSX.Element {
	return (
		<dl class="stats-overlay" aria-live="off">
			<Index each={props.stats}>
				{(stat) => (
					<div>
						<dt>{stat().label}</dt>
						<dd>{stat().value}</dd>
					</div>
				)}
			</Index>
		</dl>
	);
}
