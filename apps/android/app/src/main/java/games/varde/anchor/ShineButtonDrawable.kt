package games.varde.anchor

import android.graphics.BlurMaskFilter
import android.graphics.Canvas
import android.graphics.ColorFilter
import android.graphics.LinearGradient
import android.graphics.Paint
import android.graphics.Path
import android.graphics.PixelFormat
import android.graphics.PorterDuff
import android.graphics.PorterDuffXfermode
import android.graphics.RectF
import android.graphics.Shader
import android.graphics.drawable.Drawable

/**
 * Pixel port of the web primary button's `mouve-raised-shadow` + ::before
 * wash (HeroUI mouve-dark):
 *
 *   inset 0 1px 0 0   #fff3       hard top line, wraps the corner arcs
 *   inset 0 1px 2px -.5px #ffffff24  soft line + faint all-around halo
 *   inset 0 8px 24px -4px #ffffff1a  broad glow hugging the whole edge
 *   0 0 0 1px #0006               dark ring outside the border
 *   ::before linear-gradient(#fff → #ffffff80) @ .05  faint face wash
 *
 * A CSS inset shadow `inset ox oy blur spread` renders the area of the
 * element NOT covered by its own shape when contracted by `spread`,
 * shifted by (ox,oy) and blurred — so on a capsule each band hugs the
 * perimeter: deepest at top, wrapping around the corner arcs and fading
 * toward the lower corners. We reproduce exactly that: fill the inner
 * pill, then subtract the same pill inset by `spread` and offset by
 * `dy`, blurring the subtraction edge.
 */
class ShineButtonDrawable(
    private val fill: Int,
    private val border: Int,
    private val radiusPx: Float,
    private val px: Float,
) : Drawable() {

    private val ePath = Path()
    private val sPath = Path()
    private val rect = RectF()
    private val sRect = RectF()
    private val layerRect = RectF()
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG)
    private var userAlpha = 255
    private val clearPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply {
        xfermode = PorterDuffXfermode(PorterDuff.Mode.CLEAR)
    }

    private fun ring(d: Float): RectF = rect.apply {
        set(bounds)
        inset(d, d)
    }

    private fun pill(p: Path, r: RectF): Path {
        p.reset()
        val rad = (radiusPx - (r.left - bounds.left)).coerceAtLeast(0f)
        p.addRoundRect(r, rad, rad, Path.Direction.CW)
        return p
    }

    private fun dpPx(v: Float) = v * px

    /** band = inner pill minus the pill inset by `shrink` and offset
     *  down `dy`; `blur` feathers the band's inner edge. CSS alphas pass
     *  straight through as color alpha. */
    private fun insetBand(canvas: Canvas, dy: Float, shrink: Float, blur: Float, color: Int) {
        val inner = ring(dpPx(1.5f) - 1f)
        val save = canvas.saveLayer(
            bounds.left.toFloat(), bounds.top.toFloat(),
            bounds.right.toFloat(), bounds.bottom.toFloat(), null,
        )
        paint.reset()
        paint.isAntiAlias = true
        paint.style = Paint.Style.FILL
        paint.color = color
        paint.alpha = paint.alpha * userAlpha / 255
        canvas.drawPath(pill(ePath, inner), paint)

        sRect.set(inner)
        sRect.inset(shrink, shrink)
        sRect.offset(0f, dy)
        clearPaint.reset()
        clearPaint.isAntiAlias = true
        clearPaint.xfermode = PorterDuffXfermode(PorterDuff.Mode.CLEAR)
        if (blur > 0f) {
            clearPaint.maskFilter = BlurMaskFilter(blur, BlurMaskFilter.Blur.NORMAL)
        }
        canvas.drawPath(pill(sPath, sRect), clearPaint)
        canvas.restoreToCount(save)
    }

    override fun draw(canvas: Canvas) {
        val w = bounds.width().toFloat()
        val h = bounds.height().toFloat()
        if (w <= 0f || h <= 0f) return

        // 1) ring outside the border: 0 0 0 1px #0006
        paint.reset()
        paint.isAntiAlias = true
        paint.style = Paint.Style.STROKE
        paint.strokeWidth = dpPx(1f)
        paint.color = 0x66000000
        canvas.drawPath(pill(ePath, ring(dpPx(0.5f))), paint)

        // 2) fill pulled slightly INTO the border's inner edge so the
        // border stroke blends flush with no seam (border-box layering)
        paint.style = Paint.Style.FILL
        paint.color = fill
        canvas.drawPath(pill(ePath, ring(dpPx(1.5f) - 1f)), paint)

        // 3) accent border, 1dp centered at the 1dp inset
        paint.style = Paint.Style.STROKE
        paint.strokeWidth = dpPx(1f)
        paint.color = border
        canvas.drawPath(pill(ePath, ring(dpPx(1f))), paint)

        // 4) ::before wash: white → 50% white, 5% opacity, full face
        val inner = ring(dpPx(1.5f) - 1f)
        val saveClip = canvas.save()
        canvas.clipPath(pill(ePath, inner))
        paint.style = Paint.Style.FILL
        paint.shader = LinearGradient(
            0f, inner.top, 0f, inner.bottom,
            0x0DFFFFFF, 0x06FFFFFF, Shader.TileMode.CLAMP,
        )
        canvas.drawRect(inner.left, inner.top, inner.right, inner.bottom, paint)
        paint.shader = null
        canvas.restoreToCount(saveClip)

        // 5) inset bands, broad glow first so the sharp lines sit on top
        insetBand(canvas, dy = dpPx(8f), shrink = dpPx(4f), blur = dpPx(6f), color = 0x1AFFFFFF)
        insetBand(canvas, dy = dpPx(1f), shrink = dpPx(0.5f), blur = dpPx(1f), color = 0x24FFFFFF)
        insetBand(canvas, dy = dpPx(1f), shrink = 0f, blur = 0f, color = 0x33FFFFFF)
    }

    override fun setAlpha(alpha: Int) {
        userAlpha = alpha
        paint.alpha = alpha
    }

    override fun setColorFilter(colorFilter: ColorFilter?) {
        paint.colorFilter = colorFilter
    }

    @Deprecated("Deprecated in Java")
    override fun getOpacity(): Int = PixelFormat.TRANSLUCENT
}
