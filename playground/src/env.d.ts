/// <reference types="vite/client" />

interface ImportMetaEnv {
	readonly VITE_RELAY_URL?: string;
	// SHA-256 hash of the relay's WebTransport cert (hex). Optional in dev —
	// when unset the demo surfaces remediation guidance in the UI.
	readonly VITE_CERT_HASH?: string;
	// Base URL of the HLS egress (`qumo hls`), e.g. "http://localhost:8081".
	// Optional: unset, the egress is looked for on the page's host at port
	// 8081 (see hlsBaseUrl in config.ts).
	readonly VITE_HLS_URL?: string;
}

interface ImportMeta {
	readonly env: ImportMetaEnv;
}
