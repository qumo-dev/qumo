import {
	type BroadcastPath,
	GroupErrorCode,
	type GroupReader,
	type Session,
	SubscribeErrorCode,
	type TrackReader,
} from "@qumo/moq";
import type { Group, Track, TrackSource } from "./source.ts";

/** Adapts a `@qumo/moq` session to the player's {@link TrackSource}. */
export function moqSource(session: Session): TrackSource {
	return {
		async subscribe(broadcast, track) {
			// reason: the path is taken as typed by the user; the relay is the
			// one that accepts or rejects it.
			const [reader, err] = await session.subscribe(broadcast as BroadcastPath, track);
			if (reader === undefined) return [undefined, err];
			return [moqTrack(reader), undefined];
		},
		async rtt() {
			try {
				// getStats reports 0 when the transport gives no estimate.
				const { rtt } = await session.getStats();
				return rtt > 0 ? rtt : undefined;
			} catch {
				// reason: no estimate is an ordinary state; the caller has a fallback.
				return undefined;
			}
		},
	};
}

function moqTrack(reader: TrackReader): Track {
	return {
		async acceptGroup(signal) {
			const [group, err] = await reader.acceptGroup(signal);
			if (group === undefined) return [undefined, err];
			return [moqGroup(group), undefined];
		},
		close() {
			// TrackReader.close() only drops the local queue. closeWithError is
			// the one call that ends the subscription on the wire, and the
			// protocol has no "no error" subscribe code.
			// reason: a failure to close during teardown is not actionable.
			reader.closeWithError(SubscribeErrorCode.InternalError).catch(() => {});
		},
	};
}

function moqGroup(reader: GroupReader): Group {
	return {
		sequence: reader.sequence,
		frames: () => reader.frames(),
		cancel() {
			// reason: cancelling a stream that already ended rejects; either
			// way the group is finished with.
			reader.cancel(GroupErrorCode.ExpiredGroup).catch(() => {});
		},
	};
}
