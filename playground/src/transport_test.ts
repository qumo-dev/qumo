// The playground compiles against browser libs; tests run under Deno.
/// <reference lib="deno.ns" />
import { assertEquals } from "@std/assert";
import { chooseTransport, plainWebSocketUrl, stalls } from "./transport.ts";

const SAFARI =
	"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1";
const CHROME =
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36";

Deno.test("chooseTransport takes WebSocket on WebKit and WebTransport elsewhere", () => {
	const cases = [
		{ userAgent: SAFARI, want: "websocket" },
		{ userAgent: CHROME, want: "webtransport" },
		{ userAgent: "", want: "webtransport" },
	] as const;

	for (const c of cases) {
		assertEquals(chooseTransport(c.userAgent, "", true), c.want);
	}
});

Deno.test("chooseTransport takes the transport the page's address asks for", () => {
	const cases = [
		{ userAgent: CHROME, search: "?transport=websocket", want: "websocket" },
		{ userAgent: SAFARI, search: "?transport=webtransport", want: "webtransport" },
		{ userAgent: CHROME, search: "?x=1&transport=websocket", want: "websocket" },
		{ userAgent: CHROME, search: "?transport=WebSocket", want: "websocket" },
		{ userAgent: SAFARI, search: "?transport=WEBTRANSPORT", want: "webtransport" },
	] as const;

	for (const c of cases) {
		assertEquals(chooseTransport(c.userAgent, c.search, true), c.want);
	}
});

Deno.test("chooseTransport takes a value it does not know as no choice", () => {
	const cases = [
		{ userAgent: SAFARI, search: "?transport=carrier-pigeon", want: "websocket" },
		{ userAgent: CHROME, search: "?transport=ws", want: "webtransport" },
		{ userAgent: CHROME, search: "?transport=", want: "webtransport" },
	] as const;

	for (const c of cases) {
		assertEquals(chooseTransport(c.userAgent, c.search, true), c.want);
	}
});

Deno.test("chooseTransport takes WebTransport for an ingest, which has no WebSocket", () => {
	const cases = [
		{ userAgent: SAFARI, search: "" },
		{ userAgent: CHROME, search: "?transport=websocket" },
	] as const;

	for (const c of cases) {
		assertEquals(chooseTransport(c.userAgent, c.search, false), "webtransport");
	}
});

Deno.test("stalls is true of WebTransport on WebKit alone", () => {
	const cases = [
		{ userAgent: SAFARI, transport: "webtransport", want: true },
		{ userAgent: SAFARI, transport: "websocket", want: false },
		{ userAgent: CHROME, transport: "webtransport", want: false },
		{ userAgent: CHROME, transport: "websocket", want: false },
	] as const;

	for (const c of cases) {
		assertEquals(stalls(c.userAgent, c.transport), c.want);
	}
});

Deno.test("plainWebSocketUrl dials without TLS from http, a relay on the page's host or this machine", () => {
	const cases = [
		{
			relay: "https://localhost:4433",
			page: { protocol: "http:", hostname: "localhost" },
			want: "ws://localhost:4433/",
		},
		{
			relay: "https://192.168.1.5:4433",
			page: { protocol: "http:", hostname: "192.168.1.5" },
			want: "ws://192.168.1.5:4433/",
		},
		{
			relay: "https://127.0.0.1:4433",
			page: { protocol: "http:", hostname: "localhost" },
			want: "ws://127.0.0.1:4433/",
		},
		{
			relay: "https://[::1]:4433",
			page: { protocol: "http:", hostname: "[::1]" },
			want: "ws://[::1]:4433/",
		},
	] as const;

	for (const c of cases) {
		assertEquals(plainWebSocketUrl(c.relay, c.page), c.want);
	}
});

Deno.test("plainWebSocketUrl leaves a page on https, and a relay elsewhere, to wss", () => {
	const cases = [
		{
			relay: "https://relay.example.com:4433",
			page: { protocol: "https:", hostname: "relay.example.com" },
		},
		{
			relay: "https://localhost:4433",
			page: { protocol: "https:", hostname: "localhost" },
		},
		{
			relay: "https://relay.example.com",
			page: { protocol: "http:", hostname: "localhost" },
		},
	] as const;

	for (const c of cases) {
		assertEquals(plainWebSocketUrl(c.relay, c.page), undefined);
	}
});
