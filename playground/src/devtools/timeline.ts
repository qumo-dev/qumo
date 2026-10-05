import type { GroupRecord, GroupState } from "./recorder.ts";

// Turns a track's group records into what the timeline draws. A track with few
// groups shows each one as a bar; a track with many (audio sends tens a second)
// shows one column per second.

/** Above this many groups in view, a track is drawn as per-second columns. */
export const BAR_LIMIT = 120;

// A sender ends one group as it opens the next, and the two events can reach the
// reader in either order. An overlap this short is that hand-over, not a group
// arriving while another was still in flight.
const HANDOVER_MS = 50;

export interface Bar {
	readonly group: GroupRecord;
	/** Row the bar sits on, from 0. Bars sharing a row never overlap in time. */
	readonly lane: number;
	readonly start: number;
	readonly end: number;
}

/**
 * Places each group on the lowest lane where it does not overlap an earlier
 * one, so a track whose groups arrive one after another fills a single lane
 * and every extra lane is a group that overlapped. A hand-over between
 * consecutive groups does not count as overlap. A group still being received
 * runs to `now`.
 */
export function packLanes(groups: readonly GroupRecord[], now: number): Bar[] {
	const sorted = [...groups].sort((a, b) => a.arrived - b.arrived);
	const laneEnds: number[] = [];

	return sorted.map((group) => {
		const end = Math.max(group.ended ?? now, group.arrived);
		let lane = laneEnds.findIndex((laneEnd) => laneEnd <= group.arrived + HANDOVER_MS);
		if (lane === -1) lane = laneEnds.length;
		laneEnds[lane] = end;
		return { group, lane, start: group.arrived, end };
	});
}

export interface Column {
	/** Start of the second this column covers. */
	readonly start: number;
	/** Groups that arrived in it, by where they stand now. */
	readonly states: Readonly<Record<GroupState, number>>;
	readonly frames: number;
	readonly rendered: number;
}

/** Counts groups into `size`-millisecond columns covering [from, to). */
export function columns(
	groups: readonly GroupRecord[],
	from: number,
	to: number,
	size: number,
): Column[] {
	const count = Math.max(0, Math.ceil((to - from) / size));
	const out = Array.from({ length: count }, (_, i) => ({
		start: from + i * size,
		states: { receiving: 0, complete: 0, aborted: 0, skipped: 0, late: 0, stopped: 0 },
		frames: 0,
		rendered: 0,
	}));

	for (const group of groups) {
		const column = out[Math.floor((group.arrived - from) / size)];
		if (column === undefined) continue;
		column.states[group.state]++;
		column.frames += group.frames;
		column.rendered += group.rendered;
	}
	return out;
}
