// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { parseConfig, relayHost } from "./config.ts";

Deno.test("parseConfig keeps the relay URL and cert hash it is given", () => {
	const config = parseConfig({ relayUrl: "https://example.com:4433", certHash: "ab12" });

	assertEquals(config, { relayUrl: "https://example.com:4433", certHash: "ab12" });
});

Deno.test("parseConfig falls back to localhost for a relay URL it cannot use", () => {
	const cases = [
		{ name: "not an object", raw: "nope" },
		{ name: "null", raw: null },
		{ name: "no relay URL", raw: {} },
		{ name: "relay URL of the wrong type", raw: { relayUrl: 4433 } },
		{ name: "relay URL that does not parse", raw: { relayUrl: "example.com:4433" } },
	] as const;

	for (const c of cases) {
		assertEquals(
			parseConfig(c.raw),
			{ relayUrl: "https://localhost:4433", certHash: undefined },
			c.name,
		);
	}
});

Deno.test("parseConfig drops a cert hash that is not a string", () => {
	const config = parseConfig({ relayUrl: "https://example.com:4433", certHash: 12 });

	assertEquals(config.certHash, undefined);
});

Deno.test("relayHost is the relay URL's host without its port", () => {
	const cases = [
		{ relayUrl: "https://example.com:4433", want: "example.com" },
		{ relayUrl: "https://192.168.1.20:4433", want: "192.168.1.20" },
		{ relayUrl: "https://[::1]:4433", want: "[::1]" },
	] as const;

	for (const c of cases) {
		assertEquals(relayHost({ relayUrl: c.relayUrl }), c.want);
	}
});
