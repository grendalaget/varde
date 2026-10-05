# Varde — brand

**Status:** v1 logo, 2026-10-05

## Concept

A varde (cairn) is built by many hands and belongs to no one, just as a Varde server belongs to the group rather than one friend's PC. The mark is a hand-built cairn of hewn slate slabs. **One stone is lit (ember): that's the current host.** It sits in a two-stone tier next to a peer and under a shared capstone. On failover the light moves to another stone (see `varde-loader.svg`). The product UI can use the same idea: only the host is ever orange.

## Construction

- Five stones: base, a thin slab, a tier of two peers (the right one lit), and a cap centred on the joint between the two peers.
- Every chamfer is cut at 56°. Contact faces are parallel and every gap is constant (16/512). Corners are hewn (r≈6), and slabs alternate thick and thin (94/64/76/60).
- The favicon is a simplified three-tier version on a 16-unit pixel grid, for 32 px and below.

## Colour

| Name | Hex | Role |
|---|---|---|
| Skifer | `#1F2B34` | Stones and text on light |
| Glød | `#FF5B1F` | The host stone only |
| Papir | `#F3F0E9` | Light background |
| Natt | `#10161A` | Dark background |
| Tåke | `#DCE2E6` | Stones and text on dark |

## Type

The wordmark is "varde" in lowercase, set in Schibsted Grotesk SemiBold (SIL OFL 1.1) and outlined, with tracking of −1.2%. The top-left of the d's ascender is cut at 56°. In the lockup the x-height is 0.40 × the mark's height, the gap is 0.26 × the mark's height, and the baseline is set so the cairn's lowest corner overshoots it by the same amount as the round letters do.

## Kit files

`varde-logo(.svg|-dark|-mono)`, `varde-mark(.svg|-dark|-mono)`, `varde-wordmark.svg`, `varde-icon.svg`, `favicon.svg`, `varde-loader.svg`, plus `png/`: avatars at 512 and 1024, the 1024 icon, apple-touch-icon, favicons at 16, 32 and 48, and the lockup at 1200.

## Rules

- Light only one stone, and never recolour the others.
- Below 32 px, use the favicon instead of the full mark.
- Keep clear space equal to the cap stone's height on every side.

## Files

| File | Use |
|---|---|
| `varde-logo.svg` · `-dark` · `-mono` | Horizontal lockup for light backgrounds, dark backgrounds, and one colour (`currentColor`) |
| `varde-mark.svg` · `-dark` · `-mono` | The symbol alone, 512 × 512 |
| `varde-wordmark.svg` | The wordmark alone (`currentColor`) |
| `varde-icon.svg` | App icon (squircle tile with glow) |
| `favicon.svg` | Simplified 16-unit-grid version for 32 px and below; switches with `prefers-color-scheme` |
| `varde-loader.svg` | Animated failover: the lit stone hops between stones (honours `prefers-reduced-motion`) |
| `png/` | Avatars for GitHub and Discord, the app icon, apple-touch-icon, favicons, and the lockup |

## Using the brand

- **README header:** use `brand/svg/varde-logo.svg`, switching to `brand/svg/varde-logo-dark.svg` with `<picture>` for dark colour schemes.
- **Dashboard:** use the inline dark lockup or mark. For favicon art below 32 px, use `brand/svg/favicon.svg`.
- **Social preview:** use `brand/png/varde-logo-1200.png`.
- **Avatars:** use `brand/png/varde-avatar-*.png`.
- **Colour:** only the host is ever orange (Glød `#FF5B1F`), and only while that machine is currently hosting a server. Warnings use yellow `warn`, never Glød.
- **Minimum size:** below 32 px, use `favicon.svg`.
- **Clear space:** leave cap stone height on every side.
