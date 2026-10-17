package games.varde.anchor

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Canvas
import android.graphics.Paint
import android.graphics.PixelFormat
import android.graphics.Typeface
import android.opengl.GLES30
import android.opengl.GLSurfaceView
import android.opengl.GLUtils
import android.provider.Settings
import android.util.AttributeSet
import java.nio.ByteBuffer
import javax.microedition.khronos.egl.EGLConfig
import javax.microedition.khronos.opengles.GL10

/**
 * The varde-cloud landing-page background: a monospace glyph field lit by
 * drifting noise "clouds", click shockwaves, and the cairn mark drawn BY the
 * field. Direct port of apps/web's AsciiClouds (WebGL2) — same GLSL ES 3.0
 * program, same glyph atlas, same mark mask (baked into
 * res/drawable-nodpi/mark_mask.png at build time).
 *
 * The surface sits on the media-overlay plane (between the window
 * background — which carries the base gradient — and the view content), so
 * it must be translucent: EGL config with alpha + premultiplied output +
 * ONE/SRC_ALPHA blend, matching the canvas' premultiplied writes.
 */
class CloudView @JvmOverloads constructor(
    context: Context, attrs: AttributeSet? = null,
) : GLSurfaceView(context, attrs) {

    private val renderer = CloudRenderer(context)

    init {
        setZOrderMediaOverlay(true)
        holder.setFormat(PixelFormat.TRANSLUCENT)
        setEGLContextClientVersion(3)
        setEGLConfigChooser(8, 8, 8, 8, 0, 0)
        setRenderer(renderer)
        // value-animations-off is Android's prefers-reduced-motion: draw one
        // static frame instead of the continuous drift
        val scale = Settings.Global.getFloat(
            context.contentResolver, Settings.Global.ANIMATOR_DURATION_SCALE, 1f,
        )
        renderMode = if (scale == 0f) RENDERMODE_WHEN_DIRTY else RENDERMODE_CONTINUOUSLY
    }

    /** Mark rectangle in surface pixels, y measured from the BOTTOM. */
    fun setMarkRect(x: Float, yBottom: Float, w: Float, h: Float) {
        queueEvent { renderer.setMark(x, yBottom, w, h) }
        if (renderMode == RENDERMODE_WHEN_DIRTY) requestRender()
    }

    fun addRipple(x: Float, yBottom: Float) {
        queueEvent { renderer.pushRipple(x, yBottom) }
        // reduced-motion renders on demand only — without this a tap's
        // shockwave never gets a frame to appear in. The wave's intensity
        // fades in over ~50ms and out over ~1s, so tick for its lifetime
        // rather than one frame that may land before it's visible.
        if (renderMode == RENDERMODE_WHEN_DIRTY) {
            requestRender()
            for (i in 1..20) postDelayed({ requestRender() }, 60L * i)
        }
    }

    private class CloudRenderer(private val context: Context) : Renderer {

        val markRect = floatArrayOf(0f, 0f, 0f, 0f)
        private val ripples = FloatArray(MAX_RIPPLES * 4) { -1f }
        private var rippleSlot = 0
        private var startNanos = 0L
        private var loc = mutableMapOf<String, Int>()
        private var program = 0

        fun setMark(x: Float, y: Float, w: Float, h: Float) {
            markRect[0] = x; markRect[1] = y; markRect[2] = w; markRect[3] = h
        }

        fun pushRipple(x: Float, y: Float) {
            val t = (System.nanoTime() - startNanos) / 1e9f
            ripples[rippleSlot * 4 + 0] = x
            ripples[rippleSlot * 4 + 1] = y
            ripples[rippleSlot * 4 + 2] = t
            ripples[rippleSlot * 4 + 3] = Math.random().toFloat() * 100f
            rippleSlot = (rippleSlot + 1) % MAX_RIPPLES
        }

        override fun onSurfaceCreated(unused: GL10, config: EGLConfig) {
            // every GL object dies with the surface — recreate all of them
            program = buildProgram()
            GLES30.glUseProgram(program)
            for (n in arrayOf(
                "u_res", "u_time", "u_cell", "u_atlasN", "u_mark",
                "u_ripples[0]", "u_colBase", "u_colPeak", "u_colGlod",
                "u_atlas", "u_markTex",
            )) loc[n] = GLES30.glGetUniformLocation(program, n)

            val vao = IntArray(1)
            GLES30.glGenVertexArrays(1, vao, 0)
            GLES30.glBindVertexArray(vao[0])

            GLES30.glActiveTexture(GLES30.GL_TEXTURE0)
            uploadAtlas()
            GLES30.glActiveTexture(GLES30.GL_TEXTURE1)
            uploadMarkMask()
            GLES30.glUniform1i(loc["u_atlas"]!!, 0)
            GLES30.glUniform1i(loc["u_markTex"]!!, 1)
            GLES30.glUniform1i(loc["u_atlasN"]!!, GLYPHS.size)
            GLES30.glUniform3f(loc["u_colBase"]!!, 0.42f, 0.38f, 0.36f) // skifer
            GLES30.glUniform3f(loc["u_colPeak"]!!, 0.96f, 0.94f, 0.93f) // papir
            GLES30.glUniform3f(loc["u_colGlod"]!!, 1.0f, 0.357f, 0.122f)
            GLES30.glEnable(GLES30.GL_BLEND)
            GLES30.glBlendFunc(GLES30.GL_ONE, GLES30.GL_ONE_MINUS_SRC_ALPHA)
            startNanos = System.nanoTime()
        }

        override fun onSurfaceChanged(unused: GL10, w: Int, h: Int) {
            GLES30.glViewport(0, 0, w, h)
            GLES30.glUniform2f(loc["u_res"]!!, w.toFloat(), h.toFloat())
            GLES30.glUniform1f(
                loc["u_cell"]!!,
                (CELL_DP * context.resources.displayMetrics.density).toInt()
                    .coerceAtLeast(1).toFloat(),
            )
        }

        override fun onDrawFrame(unused: GL10) {
            GLES30.glClearColor(0f, 0f, 0f, 0f)
            GLES30.glClear(GLES30.GL_COLOR_BUFFER_BIT)
            GLES30.glUniform1f(
                loc["u_time"]!!, (System.nanoTime() - startNanos) / 1e9f,
            )
            GLES30.glUniform4f(
                loc["u_mark"]!!,
                markRect[0], markRect[1], markRect[2], markRect[3],
            )
            GLES30.glUniform4fv(loc["u_ripples[0]"]!!, MAX_RIPPLES, ripples, 0)
            GLES30.glDrawArrays(GLES30.GL_TRIANGLES, 0, 3)
        }

        private fun buildProgram(): Int {
            fun compile(type: Int, src: String): Int {
                val sh = GLES30.glCreateShader(type)
                GLES30.glShaderSource(sh, src)
                GLES30.glCompileShader(sh)
                val ok = IntArray(1)
                GLES30.glGetShaderiv(sh, GLES30.GL_COMPILE_STATUS, ok, 0)
                check(ok[0] != 0) { GLES30.glGetShaderInfoLog(sh) }
                return sh
            }
            val p = GLES30.glCreateProgram()
            GLES30.glAttachShader(p, compile(GLES30.GL_VERTEX_SHADER, VERT))
            GLES30.glAttachShader(p, compile(GLES30.GL_FRAGMENT_SHADER, FRAG))
            GLES30.glLinkProgram(p)
            val ok = IntArray(1)
            GLES30.glGetProgramiv(p, GLES30.GL_LINK_STATUS, ok, 0)
            check(ok[0] != 0) { GLES30.glGetProgramInfoLog(p) }
            return p
        }

        /** R8 glyph strip, same tiles/size as the web atlas (46px on 64). */
        private fun uploadAtlas() {
            val T = 64
            val set = GLYPHS.toMutableList()
            val paint = Paint(Paint.ANTI_ALIAS_FLAG).apply {
                typeface = Typeface.MONOSPACE
                textSize = 46f
                color = android.graphics.Color.WHITE
                textAlign = Paint.Align.CENTER
            }
            // swap runes in where the device mono font can draw them
            for ((slot, cp) in RUNE_SLOTS) {
                val rune = String(Character.toChars(cp))
                if (paint.hasGlyph(rune)) set[slot] = rune
            }

            val bmp = Bitmap.createBitmap(set.size * T, T, Bitmap.Config.ARGB_8888)
            val cv = Canvas(bmp)
            val y = T * 0.54f - (paint.descent() + paint.ascent()) / 2f
            set.forEachIndexed { i, g -> cv.drawText(g, i * T + T / 2f, y, paint) }

            // GL_R8 wants one byte per texel — take the red channel of the
            // white-on-transparent bitmap (alpha == coverage == luminance)
            val argb = IntArray(bmp.width * bmp.height)
            bmp.getPixels(argb, 0, bmp.width, 0, 0, bmp.width, bmp.height)
            val r8 = ByteBuffer.allocateDirect(argb.size)
            for (px in argb) r8.put(((px ushr 16) and 0xff).toByte())
            r8.rewind()

            val tex = IntArray(1)
            GLES30.glGenTextures(1, tex, 0)
            GLES30.glBindTexture(GLES30.GL_TEXTURE_2D, tex[0])
            for (p in intArrayOf(
                GLES30.GL_TEXTURE_MIN_FILTER, GLES30.GL_TEXTURE_MAG_FILTER,
            )) GLES30.glTexParameteri(GLES30.GL_TEXTURE_2D, p, GLES30.GL_LINEAR)
            for (p in intArrayOf(
                GLES30.GL_TEXTURE_WRAP_S, GLES30.GL_TEXTURE_WRAP_T,
            )) GLES30.glTexParameteri(GLES30.GL_TEXTURE_2D, p, GLES30.GL_CLAMP_TO_EDGE)
            GLES30.glPixelStorei(GLES30.GL_UNPACK_ALIGNMENT, 1)
            GLES30.glTexImage2D(
                GLES30.GL_TEXTURE_2D, 0, GLES30.GL_R8,
                bmp.width, bmp.height, 0,
                GLES30.GL_RED, GLES30.GL_UNSIGNED_BYTE, r8,
            )
            bmp.recycle()
        }

        private fun uploadMarkMask() {
            val opts = BitmapFactory.Options().apply { inScaled = false }
            val bmp = BitmapFactory.decodeResource(
                context.resources, R.drawable.mark_mask, opts,
            )
            val tex = IntArray(1)
            GLES30.glGenTextures(1, tex, 0)
            GLES30.glBindTexture(GLES30.GL_TEXTURE_2D, tex[0])
            for (p in intArrayOf(
                GLES30.GL_TEXTURE_MIN_FILTER, GLES30.GL_TEXTURE_MAG_FILTER,
            )) GLES30.glTexParameteri(GLES30.GL_TEXTURE_2D, p, GLES30.GL_LINEAR)
            for (p in intArrayOf(
                GLES30.GL_TEXTURE_WRAP_S, GLES30.GL_TEXTURE_WRAP_T,
            )) GLES30.glTexParameteri(GLES30.GL_TEXTURE_2D, p, GLES30.GL_CLAMP_TO_EDGE)
            GLUtils.texImage2D(GLES30.GL_TEXTURE_2D, 0, bmp, 0)
            bmp.recycle()
        }
    }

    companion object {
        private const val CELL_DP = 10
        private const val MAX_RIPPLES = 8

        // glyph ramp — identical to the web version
        private val GLYPHS = arrayOf(
            " ", "·", ":", "-", "~", "=", "+", "*",
            "x", "o", "a", "s", "c", "e", "%", "@",
        )
        private val RUNE_SLOTS = arrayOf(
            intArrayOf(9, 0x16a0), intArrayOf(11, 0x16b1), intArrayOf(13, 0x16cb),
        )

        private const val VERT = """#version 300 es
void main() {
  vec2 p = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
  gl_Position = vec4(p * 2.0 - 1.0, 0.0, 1.0);
}"""

        // verbatim port of AsciiClouds FRAG (u_ripples loop bound patched in)
        private val FRAG = """#version 300 es
precision highp float;
out vec4 fragColor;

uniform vec2  u_res;
uniform float u_time;
uniform float u_cell;
uniform sampler2D u_atlas;
uniform int   u_atlasN;
uniform vec4  u_mark;
uniform sampler2D u_markTex;
uniform vec4  u_ripples[$MAX_RIPPLES];
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

  vec2 q = cid * 0.053;
  vec2 warp = vec2(
    vnoise(q * 0.7 + vec2(t * 0.050, 0.0)),
    vnoise(q * 0.7 + vec2(0.0, t * 0.040) + 19.7));
  float v = fbm(q + warp * 0.9 + vec2(t * 0.100, t * 0.042));
  v = clamp(v + (hash21(cid) - 0.5) * 0.18, 0.0, 1.0);

  float I = 0.0;
  for (int i = 0; i < $MAX_RIPPLES; i++) {
    vec4 rp = u_ripples[i];
    float age = t - rp.z;
    if (rp.z < 0.0 || age < 0.0) continue;
    float seed = rp.w;
    float speedMul = 0.75 + fract(seed * 7.13) * 0.5;
    float sig = 52.0 * (0.7 + fract(seed * 3.71) * 0.6);
    float r = age * speedMul * max(u_res.x, u_res.y) * 0.62;
    vec2 dc = (cid + 0.5) * u_cell - rp.xy;
    float d = length(dc);
    vec2 dir = dc / max(d, 1.0);
    float wob = (vnoise(dir * 2.4 + seed * 17.3) - 0.5) * 2.0 * min(r * 0.2, 70.0);
    float band = exp(-pow(d - r - wob, 2.0) / (2.0 * sig * sig));
    float echo = exp(-pow(d - r * 0.6 - wob * 0.5, 2.0) / (2.0 * sig * sig * 0.6)) * 0.5;
    float decay = exp(-age * 1.35) * smoothstep(0.0, 0.05, age);
    float reach = exp(-r * 0.0009);
    I += (band + echo) * decay * reach;
  }
  I = clamp(I, 0.0, 1.0);
  v = min(v + I * 0.35, 1.0);

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
  col = mix(col, u_colGlod, clamp(I * 1.5, 0.0, 0.95));

  float fadeUp = smoothstep(0.02 * u_res.y, 0.40 * u_res.y, frag.y);
  float a = texture(u_atlas,
      vec2((float(idx) + cuv.x) / float(u_atlasN), 1.0 - cuv.y)).r;
  float tw = 0.86 + 0.14 * vnoise(cid * 0.31 + vec2(t * 0.50, t * 0.34));
  float alpha = a * (((vis * 0.34 + peak * 0.46) * tw + I * 1.15) * fadeUp
                     + inMark * (0.85 + I * 1.15));

  fragColor = vec4(col * alpha, alpha);
}"""
    }
}
