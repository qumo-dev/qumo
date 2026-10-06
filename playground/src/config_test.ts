// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { hlsBaseUrl, parseConfig, relayEndpoint } from "./config.ts";

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

Deno.test("relayEndpoint is the relay URL's host and port", () => {
	const cases = [
		{ relayUrl: "https://example.com:4433", want: { host: "example.com", port: 4433 } },
		{ relayUrl: "https://192.168.1.20:5000", want: { host: "192.168.1.20", port: 5000 } },
		{ relayUrl: "https://[::1]:4433", want: { host: "[::1]", port: 4433 } },
	] as const;

	for (const c of cases) {
		assertEquals(relayEndpoint({ relayUrl: c.relayUrl }), c.want);
	}
});

Deno.test("relayEndpoint takes the https port for a relay URL that names none", () => {
	const endpoint = relayEndpoint({ relayUrl: "https://example.com" });

	assertEquals(endpoint, { host: "example.com", port: 443 });
});

Deno.test("hlsBaseUrl puts the egress on the page's host, at port 8081", () => {
	const cases = [
		{ hostname: "localhost", want: "http://localhost:8081" },
		{ hostname: "192.168.1.20", want: "http://192.168.1.20:8081" },
		{ hostname: "[::1]", want: "http://[::1]:8081" },
	] as const;

	for (const c of cases) {
		assertEquals(hlsBaseUrl({ protocol: "http:", hostname: c.hostname }), c.want);
	}
});

Deno.test("hlsBaseUrl keeps http for this machine even from a page on https", () => {
	const cases = ["localhost", "app.localhost", "127.0.0.1", "[::1]"] as const;

	for (const hostname of cases) {
		assertEquals(
			hlsBaseUrl({ protocol: "https:", hostname }),
			`http://${hostname}:8081`,
			hostname,
		);
	}
});

Deno.test("hlsBaseUrl uses https from a page on https anywhere else", () => {
	const base = hlsBaseUrl({ protocol: "https:", hostname: "example.com" });

	assertEquals(base, "https://example.com:8081");
});

Deno.test("hlsBaseUrl takes an override in place of its guess", () => {
	const page = { protocol: "http:", hostname: "localhost" };
	const cases = [
		{ name: "as given", override: "https://hls.example.com", want: "https://hls.example.com" },
		{
			name: "without its trailing slash",
			override: "http://localhost:9000/",
			want: "http://localhost:9000",
		},
		{ name: "unless it is empty", override: "", want: "http://localhost:8081" },
	] as const;

	for (const c of cases) {
		assertEquals(hlsBaseUrl(page, c.override), c.want, c.name);
	}
});
