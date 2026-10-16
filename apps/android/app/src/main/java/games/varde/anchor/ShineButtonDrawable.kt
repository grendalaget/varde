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
 * The web primary button's border shine, ported edge-for-edge: a 1px
 * rgba(0,0,0,.4) ring on the outer edge, a 1px accent border inside it,
 * then white insets that hug the TOP edge only — a hard 1px line at
 * white .2, a ~3px soft sliver at white .14, and a faint ~12px glow at
 * white .1 (the measured inset stack on varde.games). No face gradient:
 * the fill below the border stays flat.
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

        // 1) dark outer ring at the very edge
        paint.style = Paint.Style.STROKE
        paint.strokeWidth = dpPx(1f)
        paint.color = 0x66000000
        canvas.drawPath(pillPath(ring(dpPx(0.5f))), paint)

        // 2) accent border inside the ring
        paint.color = border
        canvas.drawPath(pillPath(ring(dpPx(1.5f))), paint)

        // 3) flat fill inside the border
        paint.style = Paint.Style.FILL
        paint.color = fill
        canvas.drawPath(pillPath(ring(dpPx(2.5f))), paint)

        // 4) white insets, top edge only — each clipped band stroked along
        // the pill's top arc so the highlight follows the rounded corners
        paint.style = Paint.Style.STROKE
        val inner = ring(dpPx(2.5f))
        val et = inner.top // the border's inner edge — the shine hugs it
        canvas.save()
        canvas.clipRect(bounds.left.toFloat(), et, bounds.right.toFloat(), et + dpPx(1f))
        paint.strokeWidth = dpPx(1f)
        paint.color = 0x33FFFFFF
        canvas.drawPath(pillPath(inner), paint)
        canvas.restore()

        canvas.save()
        canvas.clipRect(bounds.left.toFloat(), et + dpPx(1f), bounds.right.toFloat(), et + dpPx(4.5f))
        paint.strokeWidth = dpPx(3f)
        paint.color = 0x24FFFFFF
        canvas.drawPath(pillPath(inner), paint)
        canvas.restore()

        // 5) faint top glow confined to ~12dp under the edge
        canvas.save()
        canvas.clipPath(pillPath(inner))
        paint.style = Paint.Style.FILL
        paint.shader = LinearGradient(
            0f, et, 0f, et + dpPx(12f),
            0x1AFFFFFF, 0x00FFFFFF, Shader.TileMode.CLAMP,
        )
        canvas.drawRect(bounds.left.toFloat(), et, bounds.right.toFloat(), et + dpPx(12f), paint)
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
