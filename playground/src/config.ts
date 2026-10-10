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

// The port an https URL has when it names none.
const HTTPS_PORT = 443;
// The port the playground expects the HLS egress on. `qumo hls` listens on
// 8080 unless told otherwise, which is the playground's own port, so the
// egress is started with HLS_ADDR=:8081 to go with it.
const HLS_PORT = 8081;

/** Where the relay is reached: the host, and the port it listens on. */
export interface RelayEndpoint {
	/**
	 * The host the relay and the ingest origins are reached on: the one the
	 * UI was opened at when `qumo playground` serves it, VITE_RELAY_URL's
	 * under Vite.
	 */
	host: string;
	/** The port of the relay itself; the ingest origins have their own. */
	port: number;
}

/**
 * Where the runtime config says the relay is. A relay URL that names no port
 * means 443: that is what a URL on the https port looks like once parsed.
 */
export function relayEndpoint(config: ResolvedConfig): RelayEndpoint {
	const url = new URL(config.relayUrl);
	return { host: url.hostname, port: url.port === "" ? HTTPS_PORT : Number(url.port) };
}

// Hosts that are this machine. A page on https may still fetch them over
// http: browsers leave loopback out of mixed-content blocking.
export function isLoopback(hostname: string): boolean {
	return hostname === "localhost" || hostname.endsWith(".localhost") ||
		hostname === "127.0.0.1" || hostname === "[::1]";
}

/**
 * The base URL of the HLS egress (`qumo hls`), with no trailing slash.
 *
 * The egress is a separate process that nothing tells the page about, so it
 * is taken to be on the host the page was opened at, at port 8081. Under
 * `qumo playground` that is the host the relay is on as well; under Vite it
 * is this machine, wherever the relay is.
 *
 * The egress serves plain http. A page on https can only fetch that from
 * this machine, so anywhere else the guess is https, which holds only if
 * something in front of the egress terminates TLS on that port.
 *
 * @param page - Where the page was opened: `location`, or its two fields.
 * @param override - A base URL that replaces the guess, such as VITE_HLS_URL.
 */
export function hlsBaseUrl(
	page: { protocol: string; hostname: string },
	override?: string,
): string {
	if (override !== undefined && override !== "") return override.replace(/\/+$/, "");
	const secure = page.protocol === "https:" && !isLoopback(page.hostname);
	return `${secure ? "https:" : "http:"}//${page.hostname}:${HLS_PORT}`;
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

// The build-time values, checked as /config's answer is: a VITE_RELAY_URL
// that is not an https URL would otherwise fail later, where the relay's
// address is taken apart, and leave the page connecting for ever.
function envFallback(): ResolvedConfig {
	return parseConfig({
		relayUrl: import.meta.env.VITE_RELAY_URL,
		certHash: import.meta.env.VITE_CERT_HASH,
	});
}
