// The canvas a video is drawn on, in a board. It has the size and shape of
// the picture; the stylesheet (`.video-preview canvas`) scales it to the board
// and gives it its frame.
export function PreviewCanvas(props: {
	/** The size of the picture, in pixels. */
	width: number;
	height: number;
	ref: (canvas: HTMLCanvasElement) => void;
}) {
	return (
		<canvas
			ref={props.ref}
			width={props.width}
			height={props.height}
			style={{ "aspect-ratio": `${props.width} / ${props.height}` }}
		/>
	);
}
