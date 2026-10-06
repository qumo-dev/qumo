// A source that needs no camera: a moving test picture and a tone. It is what
// to publish when checking the pipeline itself, since every frame is known:
// the picture shows its own frame number and the time it was drawn, and once
// a second it flashes as the tone beeps.

/** What the pattern is to look like. */
export interface PatternSettings {
	/** The picture's size, in pixels. */
	width: number;
	height: number;
	/** Frames drawn per second. */
	frameRate: number;
}

/** The browser could not make the canvas or the track the pattern is drawn on. */
export class PatternError extends Error {
	constructor(message: string) {
		super(message);
		this.name = "PatternError";
	}
}

// The tone, and the beep that marks each second.
const TONE_HZ = 440;
const BEEP_HZ = 880;
const TONE_GAIN = 0.03;
const BEEP_GAIN = 0.2;
const BEEP_SECONDS = 0.08;

// SMPTE-style bars, left to right.
const BARS = ["#c0c0c0", "#c0c000", "#00c0c0", "#00c000", "#c000c0", "#c00000", "#0000c0"];

// A timer on a worker. A page's own timers are slowed to one a second while
// its tab is in the background, which would freeze the picture; a worker's
// are not.
function ticker(intervalMs: number, tick: () => void): () => void {
	const source = `setInterval(() => postMessage(0), ${intervalMs});`;
	const url = URL.createObjectURL(new Blob([source], { type: "text/javascript" }));
	const worker = new Worker(url);
	worker.onmessage = tick;
	return () => {
		worker.terminate();
		URL.revokeObjectURL(url);
	};
}

function pad(value: number, digits: number): string {
	return String(value).padStart(digits, "0");
}

// The time of day to the millisecond, for reading the delay off two screens.
function clockText(date: Date): string {
	return `${pad(date.getHours(), 2)}:${pad(date.getMinutes(), 2)}:${pad(date.getSeconds(), 2)}.${
		pad(date.getMilliseconds(), 3)
	}`;
}

function draw(
	context: CanvasRenderingContext2D,
	width: number,
	height: number,
	frame: number,
	frameRate: number,
	flash: boolean,
): void {
	const barWidth = width / BARS.length;
	BARS.forEach((color, i) => {
		context.fillStyle = color;
		context.fillRect(Math.floor(i * barWidth), 0, Math.ceil(barWidth), height);
	});

	// A band the text sits on, white for the frame that begins a second.
	const band = height / 3;
	context.fillStyle = flash ? "#ffffff" : "#101010";
	context.fillRect(0, band, width, band);

	// A marker that crosses the band once a second: motion shows a stutter
	// that a still picture would hide.
	const size = band / 6;
	const travelled = (frame % frameRate) / frameRate;
	context.fillStyle = flash ? "#101010" : "#ffffff";
	context.fillRect(travelled * (width - size), band + band - size * 1.5, size, size);

	context.font = `${Math.round(band / 3)}px ui-monospace, monospace`;
	context.textBaseline = "top";
	context.fillText(clockText(new Date()), width * 0.04, band + band * 0.12);
	context.fillText(`frame ${frame}`, width * 0.04, band + band * 0.46);
}

/**
 * A media stream of the test pattern: one video track and one audio track.
 * It runs until its video track is stopped, and then lets go of everything
 * it made. Browsers hold audio until a user gesture, so this must be reached
 * from one for the tone to sound.
 *
 * @throws {PatternError} If the browser gives no canvas or no track for it.
 */
export function patternStream(settings: PatternSettings): MediaStream {
	const { width, height, frameRate } = settings;
	const canvas = document.createElement("canvas");
	canvas.width = width;
	canvas.height = height;
	const context = canvas.getContext("2d");
	if (context === null) throw new PatternError("the test pattern needs a 2D canvas");

	const audio = new AudioContext();
	const oscillator = new OscillatorNode(audio, { frequency: TONE_HZ });
	const gain = new GainNode(audio, { gain: TONE_GAIN });
	const output = audio.createMediaStreamDestination();
	oscillator.connect(gain).connect(output);
	oscillator.start();
	// reason: without a user gesture the context stays suspended; the picture
	// still runs, and the tone starts if the context is resumed later.
	audio.resume().catch(() => {});

	// With no rate given, a frame is captured each time the canvas is drawn
	// on, so the ticker below is the one thing that sets the frame rate.
	const video = canvas.captureStream().getVideoTracks()[0];
	if (video === undefined) {
		// reason: close only rejects on a context that is already closed.
		audio.close().catch(() => {});
		throw new PatternError("the test pattern canvas gave no video track");
	}

	let frame = 0;
	draw(context, width, height, frame, frameRate, true);
	const stop = ticker(1000 / frameRate, () => {
		// The track is stopped by whoever was given the stream; there is no
		// event for that, so it is looked for here.
		if (video.readyState === "ended") {
			stop();
			// reason: close only rejects on a context that is already closed.
			audio.close().catch(() => {});
			return;
		}

		frame++;
		const flash = frame % frameRate === 0;
		draw(context, width, height, frame, frameRate, flash);
		if (flash) {
			const now = audio.currentTime;
			oscillator.frequency.setValueAtTime(BEEP_HZ, now);
			gain.gain.setValueAtTime(BEEP_GAIN, now);
			oscillator.frequency.setValueAtTime(TONE_HZ, now + BEEP_SECONDS);
			gain.gain.setValueAtTime(TONE_GAIN, now + BEEP_SECONDS);
		}
	});

	return new MediaStream([video, ...output.stream.getAudioTracks()]);
}
