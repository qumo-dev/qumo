// Runtime configuration for the playground.
//
// Two code paths share this module:
//   - `qumo playground` (distribution path) serves a `/config` JSON endpoint
//     next to the UI; getConfig() fetches it so the dev cert hash is never
//     baked into the bundle and the UI never needs rebuilding when the cert
//     changes.
//   - `mage web` (Vite dev path) has no `/config` endpoint, so getConfig()
//     skips the fetch (`import.meta.env.DEV`) and reads the same import.meta.env
//     values used before this module existed — preserving the developer workflow
//     unchanged (and avoiding a noisy 404 in the console on every load).

export interface ResolvedConfig {
	/** https URL the browser dials over WebTransport, e.g. https://localhost:4433. */
	relayUrl: string;
	/** SHA-256 (hex) of the relay's WebTransport cert, or undefined when unset. */
	certHash?: string;
}

const DEFAULT_RELAY_URL = "https://localhost:4433";

/**
 * The host the relay and the ingest origins are reached on: the one the UI
 * was opened at when `qumo playground` serves it, VITE_RELAY_URL's under Vite.
 */
export function relayHost(config: ResolvedConfig): string {
	return new URL(config.relayUrl).hostname;
}

/**
 * Reads what `/config` answered. Anything it leaves out, or that is not what
 * it should be, takes the default: the UI then dials localhost, as it does
 * when run on its own machine.
 */
export function parseConfig(raw: unknown): ResolvedConfig {
	const fields: { relayUrl?: unknown; certHash?: unknown } =
		typeof raw === "object" && raw !== null ? raw : {};
	const relayUrl = typeof fields.relayUrl === "string" && isHttps(fields.relayUrl)
		? fields.relayUrl
		: DEFAULT_RELAY_URL;
	const certHash = typeof fields.certHash === "string" ? fields.certHash : undefined;
	return { relayUrl, certHash };
}

// "example.com:4433" parses too, as a URL whose scheme is "example.com:", so
// parsing alone does not show there is a host to dial.
function isHttps(url: string): boolean {
	return URL.parse(url)?.protocol === "https:";
}

let pending: Promise<ResolvedConfig> | null = null;

/** getConfig resolves the runtime config once and caches it for the session. */
export function getConfig(): Promise<ResolvedConfig> {
	if (!pending) {
		pending = resolveConfig();
	}
	return pending;
}

async function resolveConfig(): Promise<ResolvedConfig> {
	// The Vite dev server has no /config endpoint (only the built `qumo
	// playground` binary serves it), so the fetch would 404 on every dev load.
	// Skip it in dev and read import.meta.env directly; in the built UI
	// (import.meta.env.DEV === false) the /config fetch runs as before.
	if (!import.meta.env.DEV) {
		try {
			const res = await fetch("/config", {
				headers: { Accept: "application/json" },
			});
			if (res.ok) {
				const raw: unknown = await res.json();
				return parseConfig(raw);
			}
		} catch {
			// reason: a /config that cannot be fetched or is not JSON leaves
			// the build-time values below, which dial localhost.
		}
	}
	return envFallback();
}

function envFallback(): ResolvedConfig {
	return {
		relayUrl: import.meta.env.VITE_RELAY_URL ?? DEFAULT_RELAY_URL,
		certHash: import.meta.env.VITE_CERT_HASH,
	};
}
