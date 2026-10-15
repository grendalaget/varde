// Fluent design on the desktop: Microsoft's own component set, themed to
// Varde's dark violet accent (mouve --accent, oklch 70% 0.16 300 ≈ #9d7bff).
// Typeface order follows Fluent 2: Segoe UI Variable on Windows, falling
// back to the bundled Inter elsewhere.
import {
  createDarkTheme,
  type BrandVariants,
  type Theme,
} from "@fluentui/react-components";

const brand: BrandVariants = {
  10: "#0a0614",
  20: "#1d1233",
  30: "#2f1b52",
  40: "#422370",
  50: "#552b8f",
  60: "#6933ad",
  70: "#7d3fc7",
  80: "#9050dd",
  90: "#a26aec",
  100: "#b184f2",
  110: "#c09ef6",
  120: "#d0b9f9",
  130: "#dfd2fc",
  140: "#ede7fd",
  150: "#f7f3fe",
  160: "#fcfaff",
};

export const vardeTheme: Theme = {
  ...createDarkTheme(brand),
  fontFamilyBase:
    '"Segoe UI Variable Text", "Segoe UI", "Inter Variable", ui-sans-serif, system-ui, sans-serif',
};
