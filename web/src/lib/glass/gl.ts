/** Shared WebGL2 boilerplate for the glass renderers and the loupe. */

export function compileShader(
	gl: WebGL2RenderingContext,
	type: number,
	src: string,
): WebGLShader {
	const sh = gl.createShader(type)!;
	gl.shaderSource(sh, src);
	gl.compileShader(sh);
	if (!gl.getShaderParameter(sh, gl.COMPILE_STATUS)) {
		throw new Error(gl.getShaderInfoLog(sh) ?? "shader compile failed");
	}
	return sh;
}

export function linkProgram(
	gl: WebGL2RenderingContext,
	vertSrc: string,
	fragSrc: string,
): WebGLProgram {
	const prog = gl.createProgram()!;
	gl.attachShader(prog, compileShader(gl, gl.VERTEX_SHADER, vertSrc));
	gl.attachShader(prog, compileShader(gl, gl.FRAGMENT_SHADER, fragSrc));
	gl.linkProgram(prog);
	if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) {
		throw new Error(gl.getProgramInfoLog(prog) ?? "program link failed");
	}
	return prog;
}

/** One oversized triangle covering clip space; binds a_pos on the programs. */
export function bindFullscreenTriangle(
	gl: WebGL2RenderingContext,
	programs: WebGLProgram[],
): void {
	const quad = gl.createBuffer()!;
	gl.bindBuffer(gl.ARRAY_BUFFER, quad);
	gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);
	for (const prog of programs) {
		const loc = gl.getAttribLocation(prog, "a_pos");
		gl.enableVertexAttribArray(loc);
		gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0);
	}
}
