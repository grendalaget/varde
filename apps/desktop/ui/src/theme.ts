import {
  createDarkTheme,
  createLightTheme,
  type BrandVariants,
  type Theme,
} from "@fluentui/react-components";

// Fluent 2 on the Varde palette: Natt background, Skifer surfaces, Tåke
// text + primary actions (light-on-dark, like the old UI). Warnings are
// Fluent's warning intent (yellow). No Glød orange anywhere: orange marks
// "this PC is hosting" and that signal lives on the tray icon.
const brand: BrandVariants = {
  10: "#0a0e11",
  20: "#10161a",
  30: "#16202a",
  40: "#1f2b34",
  50: "#2e3c47",
  60: "#45535f",
  70: "#5f6e7a",
  80: "#82909b",
  90: "#a6b1ba",
  100: "#c0c8ce",
  110: "#d0d6da",
  120: "#dce2e6", // Tåke
  130: "#e4e9ec",
  140: "#eceff1",
  150: "#f3f5f6",
  160: "#f9fafb",
};

const dark = createDarkTheme(brand);
const light = createLightTheme(brand);

export const vardeDark: Theme = {
  ...dark,
  colorNeutralBackground1: "#10161a", // Natt
  colorNeutralBackground2: "#1f2b34", // Skifer
  colorNeutralForeground1: "#dce2e6", // Tåke
  colorNeutralForeground2: "#aebac2",
  colorNeutralForeground3: "#93a1ab",
  colorNeutralStroke1: "#2e3c47",
  colorNeutralStroke2: "#40505c",
  // Light-on-dark primary, matching the previous Tåke-filled button.
  colorBrandBackground: "#dce2e6",
  colorBrandBackgroundHover: "#e9edf0",
  colorBrandBackgroundPressed: "#c6cdd2",
  colorNeutralForegroundOnBrand: "#10161a",
  colorBrandForeground1: "#dce2e6",
  colorBrandForegroundLink: "#dce2e6",
  colorBrandForegroundLinkHover: "#ffffff",
};

// The same ramp, inverted: a pale Tåke wash for the page, white surfaces,
// Natt text, and a Skifer-filled primary button (mirror of the dark UI's
// Tåke-filled button).
export const vardeLight: Theme = {
  ...light,
  colorNeutralBackground1: "#f3f5f6",
  colorNeutralBackground2: "#ffffff",
  colorNeutralForeground1: "#10161a", // Natt
  colorNeutralForeground2: "#2e3c47",
  colorNeutralForeground3: "#5f6e7a",
  colorNeutralStroke1: "#c0c8ce",
  colorNeutralStroke2: "#a6b1ba",
  colorBrandBackground: "#1f2b34", // Skifer
  colorBrandBackgroundHover: "#2e3c47",
  colorBrandBackgroundPressed: "#10161a",
  colorNeutralForegroundOnBrand: "#dce2e6",
  colorBrandForeground1: "#1f2b34",
  colorBrandForegroundLink: "#1f2b34",
  colorBrandForegroundLinkHover: "#10161a",
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
