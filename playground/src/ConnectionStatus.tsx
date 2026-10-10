import { Show } from "solid-js";
import type { TransportKind } from "@qumo/moq";
import type { CertHashProblem } from "./cert.ts";
import { sanitizeReason } from "./errors.ts";
import { TRANSPORT_NAMES } from "./transport.ts";

// Session lifecycle as surfaced to the user (issue #134).
// "connecting" until the connect() promise settles; "connected" on success;
// "closed" when the relay ends the session gracefully mid-stream; "failed"
// with a concise reason on a handshake rejection or transport error.
export type ConnectionState = "connecting" | "connected" | "closed" | "failed";

// The label names the transport in every state: one that fails says what
// was tried, which on WebKit is not what the address shows.
const LABELS: Record<ConnectionState, (transport: string) => string> = {
	connecting: (transport) => `Connecting to relay over ${transport}…`,
	connected: (transport) => `Connected to relay over ${transport}`,
	closed: (transport) => `${transport} connection closed`,
	failed: (transport) => `${transport} connection failed`,
};

// User-facing guidance shown whenever the cert hash can't pin the relay cert.
const CERT_WARN: Record<CertHashProblem, string> = {
	missing: "Certificate hash not set — WebTransport will reject the relay's self-signed cert.",
	malformed:
		"VITE_CERT_HASH is malformed (expected 64 hex chars) — WebTransport can't pin the relay cert.",
};

// Connection status indicator: live transport state (dot + label, with the
// transport the session is on), a concise failure reason, up-front
// remediation when the cert hash is missing or malformed, and a warning for
// a session on the WebTransport that stalls.
export function ConnectionStatus(props: {
	state: ConnectionState;
	transport: TransportKind;
	/** Whether the session is on WebKit's WebTransport (see transport.ts). */
	stalls: boolean;
	error: string | null;
	certHashProblem: CertHashProblem | null;
}) {
	return (
		<div class="connection-status" data-state={props.state}>
			<span class="status-dot" />
			<span class="status-label">
				{LABELS[props.state](TRANSPORT_NAMES[props.transport])}
			</span>

			<Show when={(props.state === "failed" || props.state === "closed") && props.error}>
				<span class="status-reason">{props.error}</span>
			</Show>

			<Show when={props.stalls}>
				<span class="status-warn">
					This browser's WebTransport freezes after about 16 MB or 7,600 streams, with no
					error. The relay's own scenarios take WebSocket instead; this session cannot.
				</span>
			</Show>

			<Show when={props.certHashProblem}>
				{(problem) => (
					<span class="status-warn">
						{CERT_WARN[problem()]} Run <code>mage cert</code> and set{" "}
						<code>VITE_CERT_HASH</code> in <code>playground/.env</code>.
					</span>
				)}
			</Show>
		</div>
	);
}

// Map a raw connect failure to a concise, actionable message.
// Returns null when the failure is cert-related — ConnectionStatus already
// shows the cert remediation, so a duplicate reason would only clutter the bar.
// (When certHashProblem is null the cert hash IS configured correctly, so we
// don't mention it here — only suggest checking that the relay is reachable.)
export function friendlyConnError(
	err: unknown,
	certHashProblem: CertHashProblem | null,
): string | null {
	if (certHashProblem) return null;
	const raw = err instanceof Error ? err.message : String(err);
	const cleaned = sanitizeReason(raw, "the relay did not accept the connection");
	return `Could not connect to the relay: ${cleaned}. Check that it's running and try again.`;
}
