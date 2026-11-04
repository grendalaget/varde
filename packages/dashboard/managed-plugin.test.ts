import { expect, test } from "bun:test";
import type { Game, Schemas } from "./src/api/client";
import {
  isManagedMode,
  managedPhaseLabel,
  managedPluginError,
  managedPublicURL,
  pluginFields,
  pluginSettingsPayload,
} from "./src/lib/managedPlugin";

const field = (
  name: string,
  type: Schemas["PluginField"]["type"] = "string",
): Schemas["PluginField"] => ({
  name,
  type,
  required: false,
  hidden: false,
  secret: type === "secret",
});
const plugin: Schemas["Plugin"] = {
  id: "example",
  name: "Example plugin",
  version: "1",
  pages: [],
  settings: [
    field("address"),
    field("token", "secret"),
    field("licence", "select"),
    field("key", "secret"),
    {
      ...field("hosting", "select"),
      default: "managed",
      options: ["managed", "manual"],
    },
    { ...field("cached"), hidden: true },
  ],
  managed_server: {
    game: "example",
    mode_setting: "hosting",
    config: { licence: "licence", api_key: "key" },
    bindings: { address: "public_url", token: "secret:token" },
  },
};
const game: Game = {
  id: "example",
  name: "Example",
  os: ["linux"],
  arch: [],
  min_memory_mb: 0,
  ports: [],
  config_fields: [],
};

test("managed default hides only bindings and puts the mode first", () => {
  expect(isManagedMode(plugin, {})).toBe(true);
  expect(pluginFields(plugin, {}).map((f) => f.name)).toEqual([
    "hosting",
    "licence",
    "key",
  ]);
  expect(
    pluginFields(plugin, { hosting: "managed" }).map((f) => f.name),
  ).toEqual(["hosting", "licence", "key"]);
});

test("manual mode restores editable bindings and retains mapped config", () => {
  expect(isManagedMode(plugin, { hosting: "manual" })).toBe(false);
  expect(
    pluginFields(plugin, { hosting: "manual" }).map((f) => f.name),
  ).toEqual(["hosting", "address", "token", "licence", "key"]);
});

test("ordinary plugins retain visible fields without managed behavior", () => {
  const ordinary = { ...plugin, managed_server: undefined };
  expect(isManagedMode(ordinary, {})).toBe(false);
  expect(pluginFields(ordinary, {}).map((f) => f.name)).toEqual([
    "address",
    "token",
    "licence",
    "key",
    "hosting",
  ]);
});

test("save omits bindings, hidden fields and untouched secret inputs", () => {
  const settings = {
    address: "manual address",
    token: "",
    cached: "private",
    licence: "chosen",
    key: "",
    hosting: "managed",
  };
  expect(pluginSettingsPayload(plugin, settings, new Set())).toEqual({
    licence: "chosen",
    hosting: "managed",
  });
  expect(pluginSettingsPayload(plugin, settings, new Set(["key"]))).toEqual({
    licence: "chosen",
    hosting: "managed",
    key: "",
  });
  expect(
    pluginSettingsPayload(
      plugin,
      { ...settings, key: "replacement" },
      new Set(["key"]),
    ).key,
  ).toBe("replacement");
  expect(
    pluginSettingsPayload(
      plugin,
      { hosting: "manual", address: "existing", token: "provided" },
      new Set(["token"]),
    ),
  ).toEqual({ hosting: "manual", address: "existing", token: "provided" });
});

test("all phases use plain labels, with no stopped label", () => {
  expect(
    ["stopped", "preparing", "starting", "ready", "failed"].map((phase) =>
      managedPhaseLabel(phase as Schemas["ManagedServerStatus"]["phase"]),
    ),
  ).toEqual([
    "",
    "Downloading and preparing",
    "Starting",
    "Ready",
    "Couldn't start",
  ]);
});

test("PC errors prefer supplied OS, then catalog OS, then compatible PC", () => {
  const error = { code: "group_pc_required", message: "placement failed" };
  expect(managedPluginError(error, plugin, game)).toEqual({
    message: "Example plugin needs a Linux PC in your group.",
    needsPC: true,
  });
  expect(
    managedPluginError({ ...error, details: { os: ["windows"] } }, plugin, game)
      ?.message,
  ).toBe("Example plugin needs a Windows PC in your group.");
  expect(
    managedPluginError({ ...error, details: { required_os: "linux" } }, plugin)
      ?.message,
  ).toBe("Example plugin needs a Linux PC in your group.");
  expect(managedPluginError(error, plugin)?.message).toBe(
    "Example plugin needs a compatible PC in your group.",
  );
  expect(
    managedPluginError(error, plugin, { ...game, os: ["linux", "windows"] })
      ?.message,
  ).toBe("Example plugin needs a Linux or Windows PC in your group.");
});

test("gateway errors have operator guidance and other errors retain their message", () => {
  expect(
    managedPluginError({ code: "gateway_unconfigured" }, plugin)?.message,
  ).toBe(
    "This Varde server can't publish addresses yet. Ask the operator to set up the gateway.",
  );
  expect(
    managedPluginError({ message: "Choose a licence" }, plugin)?.message,
  ).toBe("Choose a licence");
  expect(managedPluginError(new Error("Offline"), plugin)?.message).toBe(
    "Offline",
  );
  expect(managedPluginError(null, plugin)).toBeNull();
});

test("public links only allow web addresses", () => {
  expect(managedPublicURL("https://example.test/tournament")).toBe(
    "https://example.test/tournament",
  );
  expect(managedPublicURL("javascript:alert(1)")).toBeUndefined();
  expect(managedPublicURL("not a URL")).toBeUndefined();
  expect(managedPublicURL()).toBeUndefined();
});
