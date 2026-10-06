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

export function vardeTheme(prefersDark: boolean): Theme {
  return prefersDark ? vardeDark : vardeLight;
}
