// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { pushCommandFor, pushTargetFor, relayUrlFor } from "./scenarios.ts";

Deno.test("relayUrlFor dials the relay's own port for the scenarios the relay serves", () => {
	const cases = [
		{ id: "echo", relay: { host: "localhost", port: 4433 }, want: "https://localhost:4433" },
		{
			id: "echo",
			relay: { host: "example.com", port: 5000 },
			want: "https://example.com:5000",
		},
		{ id: "hls", relay: { host: "example.com", port: 5000 }, want: "https://example.com:5000" },
	] as const;

	for (const c of cases) {
		assertEquals(relayUrlFor(c.id, c.relay), c.want);
	}
});

Deno.test("relayUrlFor dials an ingest's own port, whatever the relay's is", () => {
	const cases = [
		{ id: "rtmp", host: "example.com", want: "https://example.com:4443" },
		{ id: "rtsp", host: "[::1]", want: "https://[::1]:4543" },
		{ id: "camera", host: "localhost", want: "https://localhost:4543" },
	] as const;

	for (const c of cases) {
		assertEquals(relayUrlFor(c.id, { host: c.host, port: 5000 }), c.want);
	}
});

Deno.test("pushTargetFor points the encoder at the given host", () => {
	const cases = [
		{ id: "rtmp", want: "rtmp://example.com:1935/live/abc" },
		{ id: "rtsp", want: "rtsp://example.com:8554/live/abc" },
	] as const;

	for (const c of cases) {
		assertEquals(pushTargetFor(c.id, "/live/abc", "example.com"), c.want);
	}
});

Deno.test("pushTargetFor is empty for a scenario nothing is pushed to", () => {
	assertEquals(pushTargetFor("echo", "/live/abc", "example.com"), "");
});

Deno.test("pushCommandFor ends with the push target on the given host", () => {
	const ending = " -f flv rtmp://example.com:1935/live/abc";

	const command = pushCommandFor("rtmp", "/live/abc", "example.com");

	assertEquals(command.slice(-ending.length), ending);
});
