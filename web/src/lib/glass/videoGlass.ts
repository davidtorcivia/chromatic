/**
 * WebGL "liquid glass" for surfaces floating over the live stream.
 *
 * One renderer per bar: a single canvas spans the bar strip and draws
 * every glass surface in it (control bar, pills) in one pass, sampling
 * the room's <video> element as a GPU texture. The video frame is
 * uploaded and downsampled ONCE per frame per bar, however many
 * surfaces share it. Works identically in Safari, Chrome and Firefox.
 *
 * Optics: whole-surface magnification + rim bend (validated headlessly;
 * displacement before frost or the bend smears away), graded frost
 * (clearer at the rim), adaptive fill (darker glass over bright footage,
 * like Apple's material), a faint top-lit specular, and a ~300ms
 * "condensation" ramp when the controls appear: the glass starts clear
 * and flat and visibly freezes into focus. No chromatic dispersion,
 * ever: color fringing next to the picture is poison in a color-critical
 * tool.
 *
 * Cost control: the render loop only runs while the controls overlay is
 * visible (controls auto-hide after 3 s, so steady-state cost is zero).
 * Falls back silently to the CSS glass material when WebGL2 is missing,
 * the video isn't playing, or split screen-share mode puts other content
 * behind the bars.
 */

import { getFrameBitmap } from "./frameSource";
import { glassDisabled } from "./prefs";
import { compileShader, linkProgram, bindFullscreenTriangle } from "./gl";
import { IS_GECKO } from "$lib/platform";
import { easeOutCubic } from "$lib/video/laser";
import { getVideoContentPageRect } from "$lib/video/coordinates";

interface GroupOptions {
	/** Returns the <video> element to refract (bound lazily). */
	getVideo: () => HTMLVideoElement | null;
	/** Per-frame gate; false restores the CSS material (e.g. split mode). */
	isEnabled: () => boolean;
	/** Glass surfaces inside this bar; re-evaluated every frame. */
	items: () => HTMLElement[];
	/** Sweep request timestamp (Date.now()). Each NEW value triggers one
	 *  diagonal specular sweep, but only if the request is fresh (<500ms):
	 *  the loop pauses while the controls are hidden, so a stale request
	 *  must be absorbed, not replayed on the next reveal. Skipped under
	 *  prefers-reduced-motion. */
	shimmerAt?: () => number;
	/** Whole-surface magnification (0.08 = sample 8% toward center). */
	zoom?: number;
	/** Rim bend in px. */
	rim?: number;
	/** Rim width in px. */
	bezel?: number;
}

const MAX_RECTS = 10;
const FBO_WIDTH = 1024;
const RAMP_MS = 300;
// A few consecutive failures (driver quirks, bad video states) and the
// renderer retires permanently, restoring the CSS material — flapping
// styles every frame on a deterministic error would be worse than either.
const MAX_CONSECUTIVE_ERRORS = 3;
/* Grounding shadow while the shader owns the surface (the CSS inset
   speculars are suppressed; an outer contact shadow is still wanted). */
const CONTACT_SHADOW = "0 6px 16px rgba(0, 0, 0, 0.28), 0 1px 4px rgba(0, 0, 0, 0.22)";

const VERT = `#version 300 es
in vec2 a_pos;
out vec2 v_pos;
void main() {
  v_pos = vec2(a_pos.x * 0.5 + 0.5, 0.5 - a_pos.y * 0.5);
  gl_Position = vec4(a_pos, 0.0, 1.0);
}`;

/** Pass 1: minify the video frame into the small FBO. The vertex shader's
 *  y-flip is for screen orientation; rendering INTO a texture goes through
 *  GL's y-up framebuffer space, so flip back here to keep the FBO in the
 *  same v=0-is-top orientation as the uploaded video frame. */
const DOWNSAMPLE_FRAG = `#version 300 es
precision mediump float;
uniform sampler2D u_tex;
in vec2 v_pos;
out vec4 frag;
void main() { frag = texture(u_tex, vec2(v_pos.x, 1.0 - v_pos.y)); }`;

const GLASS_FRAG = `#version 300 es
precision highp float;
const int MAXR = ${MAX_RECTS};
uniform sampler2D u_tex;      // mipmapped downsampled scene
uniform vec2 u_canvas;        // canvas size, css px
uniform vec4 u_rects[MAXR];   // x, y, w, h in canvas css px
uniform float u_radii[MAXR];
uniform int u_count;
uniform vec2 u_uvOrigin;      // scene uv of canvas (0,0)
uniform vec2 u_uvPerPx;       // scene uv per canvas css px
uniform float u_zoom;
uniform float u_rim;
uniform float u_bezel;
uniform float u_ramp;         // 0..1 condensation
uniform float u_shimmer;      // sweep progress 0..1, or -1 when idle
in vec2 v_pos;
out vec4 frag;

float sdRR(vec2 p, vec2 b, float r) {
  vec2 q = abs(p) - b + r;
  return length(max(q, 0.0)) + min(max(q.x, q.y), 0.0) - r;
}

vec3 sceneSample(vec2 uv, float lod) {
  vec3 c = textureLod(u_tex, uv, lod).rgb;
  // Letterbox: outside the video content is black, like the page.
  float inside = step(0.0, uv.x) * step(uv.x, 1.0) * step(0.0, uv.y) * step(uv.y, 1.0);
  return c * inside;
}

void main() {
  vec2 px = v_pos * u_canvas;
  float d = 1e9;
  int idx = -1;
  for (int i = 0; i < MAXR; i++) {
    if (i >= u_count) break;
    vec2 c = u_rects[i].xy + u_rects[i].zw * 0.5;
    float di = sdRR(px - c, u_rects[i].zw * 0.5, u_radii[i]);
    if (di < d) { d = di; idx = i; }
  }
  if (d > 1.0 || idx < 0) { frag = vec4(0.0); return; }

  vec4 R = u_rects[idx];
  float rad = u_radii[idx];
  vec2 center = R.xy + R.zw * 0.5;
  vec2 b = R.zw * 0.5;
  float eps = 1.0;
  vec2 grad = vec2(
    sdRR(px + vec2(eps, 0.0) - center, b, rad) - sdRR(px - vec2(eps, 0.0) - center, b, rad),
    sdRR(px + vec2(0.0, eps) - center, b, rad) - sdRR(px - vec2(0.0, eps) - center, b, rad));
  vec2 n = normalize(grad + vec2(1e-6));

  // Lens geometry is constant; only the FROST condenses with the ramp.
  // Ramping the magnification read as content growing from the center.
  float t = clamp(-d / u_bezel, 0.0, 1.0);
  float bend = pow(1.0 - t, 1.3) * u_rim;
  vec2 disp = -(px - center) * u_zoom + n * bend;
  vec2 uv = u_uvOrigin + (px + disp) * u_uvPerPx;

  float lod = mix(mix(0.8, 2.0, u_ramp), mix(0.8, 3.4, u_ramp), t);
  vec3 c = sceneSample(uv, lod);

  // Color-critical: NO saturate/brightness boost. This glass floats over the
  // reviewed picture, so it must not shift its color — frost (the LOD blur
  // above) and the dark adaptive fill below are the only treatment.
  c = clamp(c, 0.0, 1.0);

  // Locally adaptive fill: each region of the glass responds to the
  // footage directly behind it (a bright window darkens only that end
  // of the bar). Probe the heavily downsampled scene at the fragment's
  // own position; texel span at lod 6 keeps the response smooth.
  float alum = dot(
    sceneSample(u_uvOrigin + px * u_uvPerPx, 6.0),
    vec3(0.2126, 0.7152, 0.0722));
  float fill = mix(0.36, 0.56, smoothstep(0.06, 0.6, alum));
  c = mix(c, vec3(0.086, 0.086, 0.102), fill * mix(0.85, 1.0, u_ramp));

  // Top sheen within this surface, damped over bright content
  float ly = (px.y - R.y) / max(R.w, 1.0);
  c += vec3(0.035) * smoothstep(0.55, 0.0, ly) * (1.0 - 0.5 * alum);

  // Neutral top-lit specular rim, faint, and catching more light where
  // the footage behind is bright (glass picks up ambient light)
  float rimBand = smoothstep(2.5, 0.5, -d);
  float topness = clamp(-n.y * 0.5 + 0.5, 0.0, 1.0);
  c += vec3(1.0) * rimBand * (0.02 + 0.06 * topness) * (0.6 + 0.5 * alum);

  // One-shot diagonal specular sweep (stream connect)
  if (u_shimmer >= 0.0) {
    float band = (px.x + px.y * 0.4) / (u_canvas.x + u_canvas.y * 0.4);
    float sweep = u_shimmer * 1.5 - 0.25;
    float sh = exp(-pow((band - sweep) / 0.07, 2.0));
    c += vec3(1.0) * sh * (0.07 + 0.06 * rimBand);
  }

  float alpha = clamp(0.5 - d * 0.7, 0.0, 1.0); // ~1.5px AA edge
  frag = vec4(c * alpha, alpha);
}`;

const radiusCache = new WeakMap<HTMLElement, number>();
function cornerRadius(el: HTMLElement, w: number, h: number): number {
	let r = radiusCache.get(el);
	if (r === undefined) {
		r = parseFloat(getComputedStyle(el).borderTopLeftRadius) || 8;
		radiusCache.set(el, r);
	}
	return Math.min(r, w / 2, h / 2);
}

/**
 * Svelte action for a bar container (must be position: relative). Inserts
 * one canvas spanning the bar and renders all `items()` surfaces on it.
 */
export function videoGlassGroup(node: HTMLElement, options: GroupOptions) {
	if (typeof window === "undefined" || glassDisabled()) return {};

	const opts = { zoom: 0.08, rim: 18, bezel: 26, ...options };

	const canvas = document.createElement("canvas");
	canvas.style.cssText =
		"position:absolute;inset:0;width:100%;height:100%;pointer-events:none;display:none;contain:strict";
	canvas.setAttribute("aria-hidden", "true");

	let gl: WebGL2RenderingContext | null = null;
	let glassProg: WebGLProgram;
	let downProg: WebGLProgram;
	let videoTex: WebGLTexture;
	let fboTex: WebGLTexture;
	let fbo: WebGLFramebuffer;
	const uniforms: Record<string, WebGLUniformLocation | null> = {};
	try {
		gl = canvas.getContext("webgl2", {
			alpha: true,
			premultipliedAlpha: true,
			antialias: false,
			depth: false,
			stencil: false,
		});
		if (!gl) return {};
		glassProg = linkProgram(gl, VERT, GLASS_FRAG);
		downProg = linkProgram(gl, VERT, DOWNSAMPLE_FRAG);
		bindFullscreenTriangle(gl, [glassProg, downProg]);

		videoTex = gl.createTexture()!;
		gl.bindTexture(gl.TEXTURE_2D, videoTex);
		gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
		gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
		gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
		gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);

		fboTex = gl.createTexture()!;
		fbo = gl.createFramebuffer()!;

		for (const name of [
			"u_tex", "u_canvas", "u_rects", "u_radii", "u_count",
			"u_uvOrigin", "u_uvPerPx", "u_zoom", "u_rim", "u_bezel", "u_ramp", "u_shimmer",
		]) {
			uniforms[name] = gl.getUniformLocation(glassProg, name);
		}
	} catch {
		return {};
	}

	node.insertBefore(canvas, node.firstChild);
	// A lost GPU context blanks the canvas while inline transparent styles
	// would persist on the surfaces — retire to the CSS material instead.
	canvas.addEventListener("webglcontextlost", () => retire());

	let fboW = 0;
	let fboH = 0;
	let raf = 0;
	const styled = new Set<HTMLElement>();
	const rectsArr = new Float32Array(MAX_RECTS * 4);
	const radiiArr = new Float32Array(MAX_RECTS);
	// Draw-skipping state: the glass only re-renders when something
	// actually changed (new video frame, layout move, or an animation).
	let lastUploadedBitmap: ImageBitmap | null = null;
	const prevSig = new Float32Array(MAX_RECTS * 4 + 8);
	let prevSigValid = false;

	function styleItem(el: HTMLElement, on: boolean) {
		el.style.background = on ? "transparent" : "";
		el.style.backdropFilter = on ? "none" : "";
		(el.style as unknown as Record<string, string>).webkitBackdropFilter = on ? "none" : "";
		el.style.boxShadow = on ? CONTACT_SHADOW : "";
	}

	function releaseAll() {
		canvas.style.display = "none";
		for (const el of styled) styleItem(el, false);
		styled.clear();
	}

	const overlay = node.closest(".controls-overlay");
	function overlayVisible(): boolean {
		return !overlay || overlay.classList.contains("visible");
	}

	/** Permanent retirement: restore the CSS material and stop forever. */
	function retire() {
		dead = true;
		if (raf) cancelAnimationFrame(raf);
		raf = 0;
		releaseAll();
	}

	let rampStart = 0;
	let wasRendering = false;
	let lastGeckoTick = 0;
	let consecutiveErrors = 0;
	let dead = false;
	let shimmerStart = -1;
	let prevShimmerAt = 0;
	const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

	function frame() {
		raf = 0;
		if (dead) return;
		try {
			frameBody();
			consecutiveErrors = 0;
		} catch {
			if (++consecutiveErrors >= MAX_CONSECUTIVE_ERRORS) {
				retire();
			} else {
				releaseAll();
				if (overlayVisible()) schedule();
			}
		}
	}

	function frameBody() {
		// Gecko: even the cheap skip-path bookkeeping (rect reads, item
		// diffing) is worth halving on the slowest compositor.
		if (IS_GECKO) {
			const now = performance.now();
			if (now - lastGeckoTick < 30) {
				if (overlayVisible()) schedule();
				return;
			}
			lastGeckoTick = now;
		}
		const g = gl!;
		const video = opts.getVideo();
		const usable =
			!!video &&
			video.readyState >= 2 &&
			video.videoWidth > 0 &&
			opts.isEnabled() &&
			!document.hidden &&
			// The in-app reduce-transparency toggle must take effect live
			!document.documentElement.classList.contains("reduce-transparency");
		const items = usable ? opts.items().slice(0, MAX_RECTS) : [];
		const w = node.clientWidth;
		const h = node.clientHeight;
		const content = usable ? getVideoContentPageRect(video!) : null;

		if (!usable || !items.length || !w || !h || !content) {
			wasRendering = false;
			releaseAll();
			if (overlayVisible()) schedule(); // keep polling while visible
			return;
		}
		if (!wasRendering || !overlayVisible()) rampStart = performance.now();
		wasRendering = true;

		const dpr = IS_GECKO ? 1 : Math.min(window.devicePixelRatio || 1, 2);
		const cw = Math.round(w * dpr);
		const ch = Math.round(h * dpr);
		if (canvas.width !== cw || canvas.height !== ch) {
			canvas.width = cw;
			canvas.height = ch;
		}
		canvas.style.display = "block";

		// Style-swap exactly the current item set
		for (const el of styled) {
			if (!items.includes(el)) {
				styleItem(el, false);
				styled.delete(el);
			}
		}
		for (const el of items) {
			if (!styled.has(el)) {
				styleItem(el, true);
				styled.add(el);
			}
		}

		const nodeRect = node.getBoundingClientRect();
		let count = 0;
		for (const el of items) {
			const r = el.getBoundingClientRect();
			if (!r.width || !r.height) continue;
			rectsArr[count * 4] = r.left - nodeRect.left;
			rectsArr[count * 4 + 1] = r.top - nodeRect.top;
			rectsArr[count * 4 + 2] = r.width;
			rectsArr[count * 4 + 3] = r.height;
			radiiArr[count] = cornerRadius(el, r.width, r.height);
			count++;
		}

		// Scene texture: prefer the shared downsampled bitmap (the video
		// is drawn ONCE at ~1024px for all renderers, then each uploads
		// only the small bitmap). Fall back to uploading the full frame
		// here and minifying through the FBO pass. Re-upload only when the
		// frame actually changed — frames arrive at ~30fps, the loop at 60.
		const bitmap = getFrameBitmap(video!);
		let sceneChanged = !bitmap; // legacy path re-uploads every frame
		if (bitmap && bitmap !== lastUploadedBitmap) {
			lastUploadedBitmap = bitmap;
			sceneChanged = true;
		}
		if (bitmap && sceneChanged) {
			g.bindTexture(g.TEXTURE_2D, fboTex);
			if (fboW !== bitmap.width || fboH !== bitmap.height) {
				fboW = bitmap.width;
				fboH = bitmap.height;
				g.texParameteri(g.TEXTURE_2D, g.TEXTURE_MIN_FILTER, g.LINEAR_MIPMAP_LINEAR);
				g.texParameteri(g.TEXTURE_2D, g.TEXTURE_MAG_FILTER, g.LINEAR);
				g.texParameteri(g.TEXTURE_2D, g.TEXTURE_WRAP_S, g.CLAMP_TO_EDGE);
				g.texParameteri(g.TEXTURE_2D, g.TEXTURE_WRAP_T, g.CLAMP_TO_EDGE);
			}
			g.texImage2D(g.TEXTURE_2D, 0, g.RGBA, g.RGBA, g.UNSIGNED_BYTE, bitmap);
			g.generateMipmap(g.TEXTURE_2D);
		} else if (!bitmap) {
			g.bindTexture(g.TEXTURE_2D, videoTex);
			g.texImage2D(g.TEXTURE_2D, 0, g.RGBA, g.RGBA, g.UNSIGNED_BYTE, video!);
			const targetH = Math.max(
				1,
				Math.round((FBO_WIDTH * video!.videoHeight) / video!.videoWidth),
			);
			if (fboW !== FBO_WIDTH || fboH !== targetH) {
				fboW = FBO_WIDTH;
				fboH = targetH;
				g.bindTexture(g.TEXTURE_2D, fboTex);
				g.texImage2D(g.TEXTURE_2D, 0, g.RGBA, fboW, fboH, 0, g.RGBA, g.UNSIGNED_BYTE, null);
				g.texParameteri(g.TEXTURE_2D, g.TEXTURE_MIN_FILTER, g.LINEAR_MIPMAP_LINEAR);
				g.texParameteri(g.TEXTURE_2D, g.TEXTURE_MAG_FILTER, g.LINEAR);
				g.texParameteri(g.TEXTURE_2D, g.TEXTURE_WRAP_S, g.CLAMP_TO_EDGE);
				g.texParameteri(g.TEXTURE_2D, g.TEXTURE_WRAP_T, g.CLAMP_TO_EDGE);
				g.bindFramebuffer(g.FRAMEBUFFER, fbo);
				g.framebufferTexture2D(g.FRAMEBUFFER, g.COLOR_ATTACHMENT0, g.TEXTURE_2D, fboTex, 0);
			} else {
				g.bindFramebuffer(g.FRAMEBUFFER, fbo);
			}
			g.viewport(0, 0, fboW, fboH);
			g.useProgram(downProg);
			g.bindTexture(g.TEXTURE_2D, videoTex);
			g.drawArrays(g.TRIANGLES, 0, 3);
			g.bindTexture(g.TEXTURE_2D, fboTex);
			g.generateMipmap(g.TEXTURE_2D);
		}

		// Glass pass
		g.bindTexture(g.TEXTURE_2D, fboTex);
		const ramp = easeOutCubic(Math.min((performance.now() - rampStart) / RAMP_MS, 1));
		// Sweep starts 450ms AFTER the trigger so it isn't masked by the
		// condensation ramp, which can begin on the same frame.
		const shimmerAt = opts.shimmerAt?.() ?? 0;
		if (shimmerAt !== prevShimmerAt) {
			prevShimmerAt = shimmerAt;
			if (!reducedMotion && Date.now() - shimmerAt < 500) {
				shimmerStart = performance.now() + 450;
			}
		}
		let shimmer = -1;
		if (shimmerStart >= 0) {
			const p = (performance.now() - shimmerStart) / 1100;
			if (p >= 0 && p <= 1) shimmer = easeOutCubic(p);
			else if (p > 1) shimmerStart = -1;
		}
		// Skip the draw when the output would be identical: same frame,
		// same layout, no animation running. Cuts steady-state renders to
		// the video's frame rate instead of the display's.
		const uvox = (nodeRect.left - content.left) / content.width;
		const uvoy = (nodeRect.top - content.top) / content.height;
		const sig = prevSig;
		let sigChanged = !prevSigValid || sceneChanged;
		const header = [cw, ch, count, ramp, shimmer, uvox, uvoy, 1 / content.width];
		for (let i = 0; i < 8; i++) {
			if (sig[i] !== header[i]) sigChanged = true;
			sig[i] = header[i];
		}
		for (let i = 0; i < count * 4; i++) {
			if (sig[8 + i] !== rectsArr[i]) sigChanged = true;
			sig[8 + i] = rectsArr[i];
		}
		prevSigValid = true;
		if (!sigChanged) {
			if (overlayVisible()) schedule();
			else {
				wasRendering = false;
				releaseAll();
			}
			return;
		}

		g.bindFramebuffer(g.FRAMEBUFFER, null);
		g.viewport(0, 0, cw, ch);
		g.clearColor(0, 0, 0, 0);
		g.clear(g.COLOR_BUFFER_BIT);
		g.useProgram(glassProg);
		g.uniform1i(uniforms.u_tex, 0);
		g.uniform2f(uniforms.u_canvas, w, h);
		g.uniform4fv(uniforms.u_rects, rectsArr);
		g.uniform1fv(uniforms.u_radii, radiiArr);
		g.uniform1i(uniforms.u_count, count);
		g.uniform2f(uniforms.u_uvOrigin, uvox, uvoy);
		g.uniform2f(uniforms.u_uvPerPx, 1 / content.width, 1 / content.height);
		g.uniform1f(uniforms.u_zoom, opts.zoom);
		g.uniform1f(uniforms.u_rim, opts.rim);
		g.uniform1f(uniforms.u_bezel, opts.bezel);
		g.uniform1f(uniforms.u_ramp, ramp);
		g.uniform1f(uniforms.u_shimmer, shimmer);
		g.drawArrays(g.TRIANGLES, 0, 3);

		if (overlayVisible()) schedule();
		else {
			wasRendering = false;
			releaseAll(); // overlay just hid; stop and restore CSS
		}
	}

	function schedule() {
		if (!raf) raf = requestAnimationFrame(frame);
	}

	// Wake the loop whenever the overlay becomes visible again.
	const mo = new MutationObserver(() => {
		if (overlayVisible()) schedule();
	});
	if (overlay) mo.observe(overlay, { attributes: true, attributeFilter: ["class"] });
	const onVisibility = () => {
		if (!document.hidden && overlayVisible()) schedule();
	};
	document.addEventListener("visibilitychange", onVisibility);
	schedule();

	return {
		destroy() {
			mo.disconnect();
			document.removeEventListener("visibilitychange", onVisibility);
			if (raf) cancelAnimationFrame(raf);
			releaseAll();
			canvas.remove();
			gl?.getExtension("WEBGL_lose_context")?.loseContext();
		},
	};
}
