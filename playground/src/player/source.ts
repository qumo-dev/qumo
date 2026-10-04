// The player's view of a media source. These shapes are defined here, on the
// consuming side, so the player imports no MoQ library: a transport is adapted
// to them (see moq_source.ts) rather than the player being written against one.

export type Result<T, E = Error> =
	| readonly [value: T, error: undefined]
	| readonly [value: undefined, error: E];

export interface Frame {
	/** Payload bytes. Valid only until the next frame of the same group is read. */
	readonly bytes: Uint8Array;
}

export interface Group {
	readonly sequence: number;
	/** Yields the group's frames in order. Ends when the group ends, throws if it is aborted. */
	frames(): AsyncIterable<Frame>;
	/** Gives up on the rest of the group. Safe to call on a group that has ended. */
	cancel(): void;
}

export interface Track {
	/** Waits for the next group. The wait is cancelled when `signal` resolves. */
	acceptGroup(signal: Promise<void>): Promise<Result<Group>>;
	/** Ends the subscription. */
	close(): void;
}

export interface TrackSource {
	subscribe(broadcast: string, track: string): Promise<Result<Track>>;
	/** The connection's round-trip time in milliseconds, or undefined when it is not known. */
	rtt(): Promise<number | undefined>;
}
