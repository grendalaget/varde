package games.varde.anchor

import android.graphics.Canvas
import android.graphics.ColorFilter
import android.graphics.LinearGradient
import android.graphics.Paint
import android.graphics.Path
import android.graphics.PixelFormat
import android.graphics.RectF
import android.graphics.Shader
import android.graphics.drawable.Drawable

/**
 * The web primary button's border shine, ported from measured pixels:
 * a ~1dp rgba(0,0,0,.4) ring on the outer edge, a ~1dp accent border
 * flush inside it, then white insets hugging the TOP edge only — a hard
 * ~1dp line at white .34 over the accent, a ~9dp soft fade white
 * .24→0 (measured 255,147,108 → 255,102,46 over ~9 CSS px on
 * varde.games). No face gradient: the fill below stays flat.
 */
class ShineButtonDrawable(
    private val fill: Int,
    private val border: Int,
    private val radiusPx: Float,
    private val px: Float,
) : Drawable() {

    private val path = Path()
    private val rect = RectF()
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG)

    private fun ring(d: Float): RectF = rect.apply {
        set(bounds)
        inset(d, d)
    }

    private fun pillPath(r: RectF): Path {
        path.reset()
        val rad = (radiusPx - (r.left - bounds.left)).coerceAtLeast(0f)
        path.addRoundRect(r, rad, rad, Path.Direction.CW)
        return path
    }

    private fun dpPx(v: Float) = v * px

    override fun draw(canvas: Canvas) {
        val w = bounds.width().toFloat()
        val h = bounds.height().toFloat()
        if (w <= 0f || h <= 0f) return

        // 1) dark outer ring, ~1dp on the outer edge (web: rgba(0,0,0,.4) 1px)
        paint.style = Paint.Style.STROKE
        paint.strokeWidth = dpPx(1f)
        paint.color = 0x66000000
        canvas.drawPath(pillPath(ring(dpPx(0.5f))), paint)

        // 2) flat fill pulled slightly INTO the border's inner edge — the
        // stroke drawn over it next blends flush, with no dark seam
        // between border and fill (the web layers them border-box)
        paint.style = Paint.Style.FILL
        paint.color = fill
        canvas.drawPath(pillPath(ring(dpPx(1.5f) - 1f)), paint)

        // 3) accent border on top — centered at 1dp inset, spans 0.5–1.5dp,
        // inner half antialiases onto the fill
        paint.style = Paint.Style.STROKE
        paint.color = border
        canvas.drawPath(pillPath(ring(dpPx(1f))), paint)

        // 4) hard top line, ~1dp at measured white .34 over the accent
        val inner = ring(dpPx(1.5f) - 1f)
        val et = inner.top // the fill's top edge — the shine hugs it
        canvas.save()
        canvas.clipRect(bounds.left.toFloat(), et, bounds.right.toFloat(), et + dpPx(1f))
        paint.strokeWidth = dpPx(1f)
        paint.color = 0x57FFFFFF
        canvas.drawPath(pillPath(inner), paint)
        canvas.restore()

        // 5) soft top fade, ~9dp at measured white .24→0
        canvas.save()
        canvas.clipPath(pillPath(inner))
        paint.style = Paint.Style.FILL
        paint.shader = LinearGradient(
            0f, et, 0f, et + dpPx(9f),
            0x40FFFFFF, 0x00FFFFFF, Shader.TileMode.CLAMP,
        )
        canvas.drawRect(bounds.left.toFloat(), et, bounds.right.toFloat(), et + dpPx(9f), paint)
        paint.shader = null
        canvas.restore()
    }

    override fun setAlpha(alpha: Int) {
        paint.alpha = alpha
    }

    override fun setColorFilter(colorFilter: ColorFilter?) {
        paint.colorFilter = colorFilter
    }

    @Deprecated("Deprecated in Java")
    override fun getOpacity(): Int = PixelFormat.TRANSLUCENT
}
