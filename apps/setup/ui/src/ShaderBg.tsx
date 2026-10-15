import { useEffect, useRef } from "react";

/*
 * The varde.games hero field, live: a monospace glyph field lit by
 * drifting noise "clouds". Same WebGL2 port as
 * packaging/macos/dmg-background.html (itself verbatim from
 * varde-cloud's src/components/ascii-clouds.tsx), minus the mark mask —
 * the window already renders the real mark as SVG on top — and with the
 * animation loop kept: a fixed snapshot would look like wallpaper.
 * prefers-reduced-motion (and WebGL2 absence) fall back to one frame /
 * the CSS wash alone.
 */

const CELL = 10;
const DPR_MAX = 2;
const MONO = `ui-monospace, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace`;
const GLYPHS = [
  " ", "·", ":", "-", "~", "=", "+", "*",
  "x", "o", "a", "s", "c", "e", "%", "@",
];
const RUNE_SLOTS: [number, string][] = [
  [9, "\u16A0"],
  [11, "\u16B1"],
  [13, "\u16CB"],
];

const VERT = `#version 300 es
void main() {
  vec2 p = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
  gl_Position = vec4(p * 2.0 - 1.0, 0.0, 1.0);
}`;

const FRAG = `#version 300 es
precision highp float;
out vec4 fragColor;

uniform vec2  u_res;                          // canvas size, device px
uniform float u_time;                         // seconds
uniform float u_cell;                         // cell size, device px
uniform sampler2D u_atlas;                    // R8 glyph strip
uniform int   u_atlasN;
uniform vec4  u_mark;                         // x,y,w,h device px, y from bottom
uniform sampler2D u_markTex;                  // r = stones, g = host stone
uniform vec3  u_colBase;
uniform vec3  u_colPeak;
uniform vec3  u_colGlod;

float hash21(vec2 p) {
  p = fract(p * vec2(234.34, 435.345));
  p += dot(p, p + 34.23);
  return fract(p.x * p.y);
}

float vnoise(vec2 p) {
  vec2 i = floor(p), f = fract(p);
  vec2 u = f * f * (3.0 - 2.0 * f);
  return mix(
    mix(hash21(i), hash21(i + vec2(1, 0)), u.x),
    mix(hash21(i + vec2(0, 1)), hash21(i + vec2(1, 1)), u.x),
    u.y);
}

float fbm(vec2 p) {
  return 0.55 * vnoise(p)
       + 0.30 * vnoise(p * 2.03 + 11.3)
       + 0.15 * vnoise(p * 4.07 + 27.7);
}

void main() {
  vec2 frag = gl_FragCoord.xy;
  vec2 cid  = floor(frag / u_cell);
  vec2 cuv  = fract(frag / u_cell);
  float t   = u_time;

  /* Cloud field: slow drift through domain-warped value noise. */
  vec2 q = cid * 0.053;
  vec2 warp = vec2(
    vnoise(q * 0.7 + vec2(t * 0.050, 0.0)),
    vnoise(q * 0.7 + vec2(0.0, t * 0.040) + 19.7));
  float v = fbm(q + warp * 0.9 + vec2(t * 0.100, t * 0.042));
  v = clamp(v + (hash21(cid) - 0.5) * 0.18, 0.0, 1.0);

  /* Mark mask: unused here — u_mark.z stays 0 and the branch is dead. */
  vec2 mk = vec2(0.0);
  if (u_mark.z > 0.0) {
    vec2 muv = (frag - u_mark.xy) / u_mark.zw;
    if (muv.x > 0.0 && muv.x < 1.0 && muv.y > 0.0 && muv.y < 1.0)
      mk = texture(u_markTex, vec2(muv.x, 1.0 - muv.y)).rg;
  }
  float inMark = mk.r + mk.g;
  float inHost = mk.g;
  float markV = 0.78 + 0.22 * vnoise(cid * 0.22 + vec2(t * 0.05, -t * 0.04));
  v = mix(v, markV, inMark);

  float vis  = smoothstep(0.36, 0.60, v);
  float peak = smoothstep(0.72, 0.94, v);
  float h = hash21(cid * 1.31 + 7.7);
  int idx = clamp(
    1 + int(floor((h * 0.6 + v * 0.5) * float(u_atlasN - 1))),
    1, u_atlasN - 1);

  vec3 col = mix(u_colBase, u_colPeak, peak);
  col = mix(col, mix(u_colBase, u_colPeak, 0.45 + 0.55 * peak), inMark);
  col = mix(col, u_colGlod, inHost);

  float fadeUp = smoothstep(0.02 * u_res.y, 0.40 * u_res.y, frag.y);
  float a = texture(u_atlas,
      vec2((float(idx) + cuv.x) / float(u_atlasN), 1.0 - cuv.y)).r;
  float tw = 0.86 + 0.14 * vnoise(cid * 0.31 + vec2(t * 0.50, t * 0.34));
  float alpha = a * (((vis * 0.34 + peak * 0.46) * tw) * fadeUp
                     + inMark * 0.85);

  fragColor = vec4(col * alpha, alpha);   // premultiplied
}`;

const COL = {
  base: [0.42, 0.38, 0.36],
  peak: [0.96, 0.94, 0.93],
  glod: [1.0, 0.357, 0.122],
};

function drawGlyphProbe(ch: string, font: string): string {
  const c = document.createElement("canvas");
  c.width = c.height = 32;
  const x = c.getContext("2d", { willReadFrequently: true })!;
  x.font = font;
  x.textAlign = "center";
  x.textBaseline = "middle";
  x.fillStyle = "#fff";
  x.fillText(ch, 16, 17);
  return x.getImageData(0, 0, 32, 32).data.join(",");
}

function resolveGlyphSet(): string[] {
  const set = [...GLYPHS];
  try {
    const font = `500 44px ${MONO}`;
    const tofu = drawGlyphProbe("\u{10FFFE}", font);
    for (const [slot, rune] of RUNE_SLOTS) {
      const img = drawGlyphProbe(rune, font);
      if (img !== tofu && img !== drawGlyphProbe(" ", font)) set[slot] = rune;
    }
  } catch {
    /* keep latin set */
  }
  return set;
}

function makeAtlas(gl: WebGL2RenderingContext): WebGLTexture {
  const set = resolveGlyphSet();
  const T = 64;
  const c = document.createElement("canvas");
  c.width = set.length * T;
  c.height = T;
  const x = c.getContext("2d")!;
  x.font = `500 46px ${MONO}`;
  x.textAlign = "center";
  x.textBaseline = "middle";
  x.fillStyle = "#fff";
  set.forEach((g, i) => x.fillText(g, i * T + T / 2, T * 0.54));

  const tex = gl.createTexture()!;
  gl.bindTexture(gl.TEXTURE_2D, tex);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
  gl.texImage2D(gl.TEXTURE_2D, 0, gl.R8, gl.RED, gl.UNSIGNED_BYTE, c);
  return tex;
}

function compile(gl: WebGL2RenderingContext, type: number, src: string) {
  const sh = gl.createShader(type)!;
  gl.shaderSource(sh, src);
  gl.compileShader(sh);
  if (!gl.getShaderParameter(sh, gl.COMPILE_STATUS))
    throw new Error(gl.getShaderInfoLog(sh) || "shader compile failed");
  return sh;
}

export default function ShaderBg() {
  const ref = useRef<HTMLCanvasElement>(null);

  useEffect(() => {
    const canvas = ref.current;
    if (!canvas) return;
    const gl = canvas.getContext("webgl2", {
      alpha: true,
      antialias: false,
      depth: false,
      stencil: false,
      powerPreference: "low-power",
    });
    if (!gl) return; // CSS wash alone is the fallback

    const program = gl.createProgram()!;
    gl.attachShader(program, compile(gl, gl.VERTEX_SHADER, VERT));
    gl.attachShader(program, compile(gl, gl.FRAGMENT_SHADER, FRAG));
    gl.linkProgram(program);
    if (!gl.getProgramParameter(program, gl.LINK_STATUS)) return;
    gl.useProgram(program);

    const U = (n: string) => gl.getUniformLocation(program, n);
    const atlas = makeAtlas(gl);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, atlas);
    gl.uniform1i(U("u_atlas"), 0);
    gl.uniform1i(U("u_atlasN"), GLYPHS.length);
    // mark texture never sampled (u_mark.z = 0) but bind it anyway
    gl.activeTexture(gl.TEXTURE1);
    gl.bindTexture(gl.TEXTURE_2D, atlas);
    gl.uniform1i(U("u_markTex"), 1);
    gl.uniform4f(U("u_mark"), 0, 0, 0, 0);
    gl.uniform3fv(U("u_colBase"), COL.base);
    gl.uniform3fv(U("u_colPeak"), COL.peak);
    gl.uniform3fv(U("u_colGlod"), COL.glod);

    // rAF can be starved in this embedded WebView2 (renderer treats the
    // frameless window as non-animating), so drive the field off a timer.
    let timer: ReturnType<typeof setInterval> | null = null;
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)");
    const t0 = performance.now();
    let tick = 0;

    const frame = () => {
      const dpr =
        Math.floor(Math.min(devicePixelRatio || 1, DPR_MAX) * 4) / 4 || 1;
      const r = canvas.getBoundingClientRect();
      const w = Math.round(r.width * dpr);
      const h = Math.round(r.height * dpr);
      if (w > 0 && h > 0 && (canvas.width !== w || canvas.height !== h)) {
        canvas.width = w;
        canvas.height = h;
        gl.viewport(0, 0, w, h);
        gl.uniform2f(U("u_res"), w, h);
        gl.uniform1f(U("u_cell"), Math.round(CELL * dpr));
      }
      if (w > 0 && h > 0) {
        gl.uniform1f(U("u_time"), 8.5 + (performance.now() - t0) / 1000);
        gl.drawArrays(gl.TRIANGLES, 0, 3);
      }
    };

    const start = () => {
      if (timer !== null) return;
      tick = 0;
      timer = setInterval(() => {
        tick += 1;
        frame();
      }, 33);
    };
    const stop = () => {
      if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
    };

    frame();
    if (!reduced.matches) start();
    const onChange = () => {
      if (reduced.matches) {
        stop();
        frame();
      } else {
        start();
      }
    };
    reduced.addEventListener("change", onChange);
    return () => {
      stop();
      reduced.removeEventListener("change", onChange);
      gl.getExtension("WEBGL_lose_context")?.loseContext();
    };
  }, []);

  return (
    <div className="pointer-events-none absolute inset-0 overflow-hidden" aria-hidden>
      {/* same field base as the DMG backdrop: Natt + the two washes */}
      <div
        className="absolute inset-0"
        style={{
          background:
            "radial-gradient(ellipse 60% 50% at 70% 20%, rgba(255,91,31,0.05), transparent 70%)," +
            "radial-gradient(ellipse 50% 40% at 20% 80%, rgba(56,49,47,0.35), transparent 70%)",
        }}
      />
      <canvas ref={ref} className="absolute inset-0 h-full w-full" />
      {/* keep the bottom strip + cards readable over the field */}
      <div className="absolute inset-0 bg-gradient-to-b from-transparent via-transparent to-background/70" />
    </div>
  );
}
