// Which transport the page reaches the relay on.
import { isWebKit, type TransportKind } from "@qumo/moq";
import { isLoopback } from "./config.ts";

/** A transport's name, as the page shows it. */
export const TRANSPORT_NAMES: Readonly<Record<TransportKind, string>> = {
	webtransport: "WebTransport",
	websocket: "WebSocket",
};

/**
 * Chooses the transport for a session.
 *
 * WebKit's WebTransport stalls after about 7,600 streams or 16 MB
 * (https://bugs.webkit.org/show_bug.cgi?id=319818), so a WebKit browser
 * takes WebSocket. `?transport=websocket` or `?transport=webtransport` in
 * the page's address overrides that, to try either on any browser; any
 * other value is no choice.
 *
 * Only the relay takes WebSocket. An ingest on a port of its own takes
 * WebTransport alone, whatever is asked for.
 *
 * @param userAgent - `navigator.userAgent`.
 * @param search - `location.search`.
 * @param relayServed - Whether the session is with the relay itself.
 */
export function chooseTransport(
	userAgent: string,
	search: string,
	relayServed: boolean,
): TransportKind {
	if (!relayServed) return "webtransport";
	const asked = new URLSearchParams(search).get("transport")?.toLowerCase();
	if (asked === "websocket" || asked === "webtransport") return asked;
	return isWebKit(userAgent) ? "websocket" : "webtransport";
}

/**
 * Reports whether a session is on the WebTransport that stalls: WebKit's.
 * It connects and plays, then freezes with no error, so the page says so
 * up front. It happens where there is no WebSocket to take, at an ingest,
 * and where the address asked for WebTransport.
 */
export function stalls(userAgent: string, transport: TransportKind): boolean {
	return transport === "webtransport" && isWebKit(userAgent);
}

/**
 * Where to dial WebSocket when it is not where `connect` looks by itself,
 * which is the relay's URL with `wss:`.
 *
 * A relay on its development certificate cannot be reached that way:
 * WebTransport pins the certificate by its hash, and WebSocket cannot. The
 * relay takes WebSocket without TLS on the same port, so that is dialed
 * instead. A page may only do so from plain http, and it is only done for
 * a relay on the page's own host or on this machine: any other relay is
 * one with a certificate browsers trust, and is not to be sent media in
 * the clear.
 *
 * @param relayUrl - The relay's `https:` URL.
 * @param page - Where the page was opened: `location`, or its two fields.
 * @returns The `ws:` URL, or undefined to leave it to `connect`.
 */
export function plainWebSocketUrl(
	relayUrl: string,
	page: { protocol: string; hostname: string },
): string | undefined {
	if (page.protocol !== "http:") return undefined;
	const url = new URL(relayUrl);
	if (url.hostname !== page.hostname && !isLoopback(url.hostname)) return undefined;
	url.protocol = "ws:";
	return url.href;
}
