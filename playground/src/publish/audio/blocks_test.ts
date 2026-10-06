// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { Blocks } from "./blocks.ts";

// A quantum of `length` samples counting up from `from`, on each channel
// offset by 100 per channel so the channels can be told apart.
function quantum(from: number, length: number, channels: number): Float32Array[] {
	return Array.from(
		{ length: channels },
		(_, channel) => Float32Array.from({ length }, (_, i) => from + i + channel * 100),
	);
}

function gather(blocks: Blocks, quanta: readonly Float32Array[][]): number[][] {
	const emitted: number[][] = [];
	for (const q of quanta) blocks.push(q, (block) => emitted.push([...block]));
	return emitted;
}

Deno.test("Blocks emits nothing until a block is full", () => {
	const blocks = new Blocks(1, 4);

	const emitted = gather(blocks, [quantum(0, 3, 1)]);

	assertEquals(emitted, []);
});

Deno.test("Blocks carries what is left of a quantum into the next block", () => {
	const blocks = new Blocks(1, 4);

	const emitted = gather(blocks, [quantum(0, 3, 1), quantum(3, 3, 1), quantum(6, 3, 1)]);

	assertEquals(emitted, [[0, 1, 2, 3], [4, 5, 6, 7]]);
});

Deno.test("Blocks emits every block a long quantum completes", () => {
	const blocks = new Blocks(1, 2);

	const emitted = gather(blocks, [quantum(0, 5, 1)]);

	assertEquals(emitted, [[0, 1], [2, 3]]);
});

Deno.test("Blocks lays a block out one channel after another", () => {
	const blocks = new Blocks(2, 3);

	const emitted = gather(blocks, [quantum(0, 3, 2)]);

	assertEquals(emitted, [[0, 1, 2, 100, 101, 102]]);
});

Deno.test("Blocks gives a channel the input lacks the first channel's samples", () => {
	const blocks = new Blocks(2, 2);

	const emitted = gather(blocks, [quantum(7, 2, 1)]);

	assertEquals(emitted, [[7, 8, 7, 8]]);
});

Deno.test("Blocks ignores a quantum with no channels", () => {
	const blocks = new Blocks(2, 2);

	const emitted = gather(blocks, [[], quantum(0, 2, 2)]);

	assertEquals(emitted, [[0, 1, 100, 101]]);
});

Deno.test("Blocks starts the next block afresh after a reset", () => {
	const blocks = new Blocks(1, 3);
	gather(blocks, [quantum(0, 2, 1)]);

	blocks.reset();
	const emitted = gather(blocks, [quantum(10, 3, 1)]);

	assertEquals(emitted, [[10, 11, 12]]);
});
