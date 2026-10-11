import {
  createDarkTheme,
  createLightTheme,
  type BrandVariants,
  type Theme,
} from "@fluentui/react-components";

// Fluent 2 on the Varde design system (HeroUI Pro "mouve" theme,
// cc850839): warm-tinted neutrals (#110C0A family, oklch hue ~38), snow
// text + primary actions (light-on-dark, like the old UI). Warnings are
// Fluent's warning intent (yellow). No Glød orange anywhere: orange marks
// "this PC is hosting" and that signal lives on the tray icon.
const brand: BrandVariants = {
  10: "#0a0706",
  20: "#110c0a",
  30: "#1a1513",
  40: "#231e1c",
  50: "#2b2523",
  60: "#38312f",
  70: "#44403e",
  80: "#5e5a58",
  90: "#7c7573",
  100: "#958d8b",
  110: "#c9c1be",
  120: "#ebe5e3",
  130: "#f4efed",
  140: "#f9f6f4",
  150: "#fcfcfc",
  160: "#ffffff",
};

// Design-system typeface is Inter; the variable build ships with the app.
const FONT_FAMILY =
  '"Inter Variable", "Inter", "Segoe UI", system-ui, sans-serif';
// DS shape language is rounder than Fluent defaults (16px basis).
const RADII = {
  borderRadiusSmall: "4px",
  borderRadiusMedium: "8px",
  borderRadiusLarge: "12px",
  borderRadiusXLarge: "16px",
};

const dark = createDarkTheme(brand);
const light = createLightTheme(brand);

export const vardeDark: Theme = {
  ...dark,
  ...RADII,
  fontFamilyBase: FONT_FAMILY,
  colorNeutralBackground1: "#110c0a", // mouve background
  colorNeutralBackground2: "#1a1513", // mouve surface
  colorNeutralForeground1: "#fcfcfc", // mouve foreground (snow)
  colorNeutralForeground2: "#c9c1be",
  colorNeutralForeground3: "#958d8b", // mouve muted
  colorNeutralStroke1: "#322c2a", // mouve border
  colorNeutralStroke2: "#44403e", // mouve border-secondary
  // Light-on-dark primary, matching the previous snow-filled button.
  colorBrandBackground: "#fcfcfc",
  colorBrandBackgroundHover: "#ffffff",
  colorBrandBackgroundPressed: "#c9c1be",
  colorNeutralForegroundOnBrand: "#110c0a",
  colorBrandForeground1: "#fcfcfc",
  colorBrandForegroundLink: "#fcfcfc",
  colorBrandForegroundLinkHover: "#ffffff",
};

// The same ramp, inverted: a pale papir wash for the page, snow surfaces,
// eclipse text, and a dark-warm primary button (mirror of the dark UI's
// snow-filled button).
export const vardeLight: Theme = {
  ...light,
  ...RADII,
  fontFamilyBase: FONT_FAMILY,
  colorNeutralBackground1: "#f4efed", // mouve-light background
  colorNeutralBackground2: "#fcfcfc", // snow
  colorNeutralForeground1: "#26201e", // mouve-light foreground (eclipse)
  colorNeutralForeground2: "#44403e",
  colorNeutralForeground3: "#5e5a58",
  colorNeutralStroke1: "#c9c1be",
  colorNeutralStroke2: "#958d8b",
  colorBrandBackground: "#26201e", // eclipse
  colorBrandBackgroundHover: "#38312f",
  colorBrandBackgroundPressed: "#110c0a",
  colorNeutralForegroundOnBrand: "#fcfcfc",
  colorBrandForeground1: "#26201e",
  colorBrandForegroundLink: "#26201e",
  colorBrandForegroundLinkHover: "#110c0a",
};

// Fluent's ramps are 16 shades around a key color at index 80. Build one
// from the OS accent color: indices below 80 mix toward black, above mix
// toward white (approximates what the Fluent theme designer emits).
const RAMP_STEPS = [
  10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160,
];

function parseColor(c: string): [number, number, number] | null {
  const m = /^(?:#([0-9a-f]{6})|rgb\(\s*(\d+),\s*(\d+),\s*(\d+)\s*\))$/i.exec(
    c,
  );
  if (!m) return null;
  if (m[1]) {
    const n = parseInt(m[1], 16);
    return [(n >> 16) & 0xff, (n >> 8) & 0xff, n & 0xff];
  }
  return [Number(m[2]), Number(m[3]), Number(m[4])];
}

function toHex(rgb: [number, number, number]): string {
  return "#" + rgb.map((v) => v.toString(16).padStart(2, "0")).join("");
}

export function brandFromAccent(accent: string): BrandVariants | null {
  const rgb = parseColor(accent);
  if (!rgb) return null;
  const mix = (other: [number, number, number], t: number): string =>
    toHex(
      rgb.map((v, i) => Math.round(v + (other[i] - v) * t)) as [
        number,
        number,
        number,
      ],
    );
  const ramp = {} as BrandVariants;
  for (const step of RAMP_STEPS) {
    ramp[step as keyof BrandVariants] =
      step <= 80
        ? mix([0, 0, 0], ((80 - step) / 80) * 0.8)
        : mix([255, 255, 255], ((step - 80) / 80) * 0.9);
  }
  ramp[80] = toHex(rgb);
  return ramp;
}

const BRAND_TOKEN_KEYS = [
  "colorBrandBackground",
  "colorBrandBackgroundHover",
  "colorBrandBackgroundPressed",
  "colorBrandForeground1",
  "colorBrandForeground2",
  "colorBrandForegroundLink",
  "colorBrandForegroundLinkHover",
  "colorNeutralForegroundOnBrand",
  "colorCompoundBrandForeground1",
  "colorCompoundBrandForeground1Hover",
  "colorCompoundBrandForeground1Pressed",
  "colorCompoundBrandBackground",
  "colorCompoundBrandBackgroundHover",
  "colorCompoundBrandBackgroundPressed",
] as const;

export function vardeTheme(
  prefersDark: boolean,
  accent?: string | null,
  glass = false,
): Theme {
  const base = prefersDark ? vardeDark : vardeLight;
  const accentRgb = accent ? parseColor(accent) : null;
  const accentRamp = accentRgb ? brandFromAccent(accent as string) : null;
  if (!accentRamp && !glass) return base;

  let theme = { ...base };
  if (accentRamp && accentRgb) {
    // OS accent owns the brand/compound tokens (buttons, links,
    // checkboxes) like WinUI's SystemAccentColor; neutrals stay Varde
    const themed = prefersDark
      ? createDarkTheme(accentRamp)
      : createLightTheme(accentRamp);
    for (const key of BRAND_TOKEN_KEYS) {
      theme = { ...theme, [key]: themed[key] };
    }
    // Fluent always paints colorNeutralForegroundOnBrand white — on a
    // pale accent that erases primary-button labels, so pick black or
    // white by the accent's perceived luminance instead.
    const lum =
      (0.2126 * accentRgb[0] + 0.7152 * accentRgb[1] + 0.0722 * accentRgb[2]) /
      255;
    theme = {
      ...theme,
      colorNeutralForegroundOnBrand: lum > 0.5 ? "#1a1a1a" : "#ffffff",
    };
  }
  if (glass) {
    // the window sits over a Mica/acrylic backdrop; let it show through
    theme = { ...theme, colorNeutralBackground1: "transparent" };
  }
  return theme;
}
