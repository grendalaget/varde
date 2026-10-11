/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Varde design system (HeroUI Pro "mouve", cc850839): warm-tinted
        // neutrals. Glød is reserved for the current host.
        natt: "#110C0A",
        skifer: {
          DEFAULT: "#231E1C",
          50: "#FCFCFC",
          100: "#EBE5E3",
          200: "#C9C1BE",
          300: "#958D8B",
          400: "#8B8380",
          500: "#857D7A",
          600: "#44403E",
          700: "#38312F",
          800: "#231E1C",
          900: "#1A1513",
          950: "#110C0A",
        },
        take: "#FCFCFC",
        papir: "#F4EFED",
        glod: "#FF5B1F",
        warn: "#F7B750",
      },
      fontFamily: {
        sans: [
          '"Inter Variable"',
          '"Inter"',
          "ui-sans-serif",
          "system-ui",
          "sans-serif",
        ],
        display: [
          '"Inter Variable"',
          '"Inter"',
          "ui-sans-serif",
          "system-ui",
          "sans-serif",
        ],
      },
      transitionTimingFunction: {
        "vd-out": "cubic-bezier(0.22, 1, 0.36, 1)",
        "vd-spring": "cubic-bezier(0.34, 1.4, 0.64, 1)",
      },
    },
  },
  plugins: [],
};
