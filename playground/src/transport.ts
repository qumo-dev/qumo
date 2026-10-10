// Which transport the page reaches the relay on.
import { isWebKit, type TransportKind } from "@qumo/moq";

/**
 * Chooses the transport for a session.
 *
 * WebKit's WebTransport stalls after about 7,600 streams or 16 MB
 * (https://bugs.webkit.org/show_bug.cgi?id=319818), so a WebKit browser
 * takes WebSocket. `?transport=websocket` or `?transport=webtransport` in
 * the page's address overrides that, to try either on any browser.
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
	const asked = new URLSearchParams(search).get("transport");
	if (asked === "websocket" || asked === "webtransport") return asked;
	return isWebKit(userAgent) ? "websocket" : "webtransport";
}

/**
 * Where the relay takes WebSocket, given its `https:` URL.
 *
 * A page on plain http is the relay on a development certificate, which
 * WebTransport pins by its hash and WebSocket cannot. The relay takes
 * WebSocket without TLS on the same port, so that is dialed. A page on
 * https may not open `ws:`, and its relay has a certificate browsers trust.
 *
 * @param relayUrl - The relay's `https:` URL.
 * @param pageProtocol - `location.protocol`.
 */
export function webSocketUrlFor(relayUrl: string, pageProtocol: string): string {
	const url = new URL(relayUrl);
	url.protocol = pageProtocol === "http:" ? "ws:" : "wss:";
	return url.href;
}
