// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { FakeTime } from "@std/testing/time";
import { Pacer } from "./pacer.ts";

// A pacer that records what it released and what it discarded.
function recording(): { pacer: Pacer<string>; emitted: string[]; dropped: string[] } {
	const emitted: string[] = [];
	const dropped: string[] = [];
	const pacer = new Pacer<string>((item) => emitted.push(item), (item) => dropped.push(item));
	return { pacer, emitted, dropped };
}

Deno.test("releases an item with no delay at once", () => {
	using _time = new FakeTime();
	const { pacer, emitted } = recording();

	pacer.push("a", 0);

	assertEquals(emitted, ["a"]);
});

Deno.test("holds an item until its delay has passed", () => {
	using time = new FakeTime();
	const { pacer, emitted } = recording();
	pacer.push("a", 50);

	time.tick(49);
	const before = emitted.slice();
	time.tick(1);

	assertEquals(before, []);
	assertEquals(emitted, ["a"]);
});

Deno.test("releases items in the order pushed when their delays ask otherwise", () => {
	using time = new FakeTime();
	const { pacer, emitted } = recording();
	pacer.push("a", 80);
	pacer.push("b", 20);

	time.tick(20);

	assertEquals(emitted, ["a", "b"]);
});

Deno.test("releases held items before one pushed with no delay", () => {
	using _time = new FakeTime();
	const { pacer, emitted } = recording();
	pacer.push("a", 80);

	pacer.push("b", 0);

	assertEquals(emitted, ["a", "b"]);
});

Deno.test("releases each item once", () => {
	using time = new FakeTime();
	const { pacer, emitted } = recording();
	pacer.push("a", 80);
	pacer.push("b", 20);

	time.tick(200);

	assertEquals(emitted, ["a", "b"]);
});

Deno.test("clear discards held items without releasing them", () => {
	using time = new FakeTime();
	const { pacer, emitted, dropped } = recording();
	pacer.push("a", 50);
	pacer.push("b", 60);

	pacer.clear();
	time.tick(100);

	assertEquals(emitted, []);
	assertEquals(dropped, ["a", "b"]);
});
