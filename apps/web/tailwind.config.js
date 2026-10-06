/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Brand v1 (brand/README.md). Glød is reserved for the current host.
        natt: "#10161A",
        skifer: {
          DEFAULT: "#1F2B34",
          50: "#EEF0F2",
          100: "#DCE2E6",
          200: "#C2C8CD",
          300: "#A7AFB4",
          400: "#899198",
          500: "#7A8389",
          600: "#545F66",
          700: "#364149",
          800: "#1F2B34",
          900: "#182027",
          950: "#10161A",
        },
        take: "#DCE2E6",
        papir: "#F3F0E9",
        glod: "#FF5B1F",
        warn: "#E3C04B",
      },
      fontFamily: {
        display: [
          '"Schibsted Grotesk"',
          "ui-sans-serif",
          "system-ui",
          "sans-serif",
        ],
      },
    },
  },
  plugins: [],
};
