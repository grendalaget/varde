import { expect, test } from "bun:test";
import { configDefaults } from "./src/lib/config-defaults";

test("omits null and missing defaults", () => {
  expect(
    configDefaults([
      { name: "password", default: null },
      { name: "gslt", default: null },
      { name: "server_name" },
    ]),
  ).toEqual({});
});

test("preserves non-null defaults including falsy values", () => {
  expect(
    configDefaults([
      { name: "password", default: "" },
      { name: "bots", default: 0 },
      { name: "public", default: false },
      { name: "map", default: "de_dust2" },
    ]),
  ).toEqual({ password: "", bots: 0, public: false, map: "de_dust2" });
});

test("empty fields have no defaults", () => {
  expect(configDefaults([])).toEqual({});
});
