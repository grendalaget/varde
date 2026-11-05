import { errorMessage, type Game, type Schemas } from "../api/client";

type Plugin = Schemas["Plugin"];

export function isManagedMode(
  plugin: Plugin,
  settings: Record<string, unknown>,
) {
  const managed = plugin.managed_server;
  if (!managed) return false;
  const mode = plugin.settings.find(
    (field) => field.name === managed.mode_setting,
  );
  return (settings[managed.mode_setting] ?? mode?.default) === mode?.default;
}

export function pluginFields(
  plugin: Plugin,
  settings: Record<string, unknown>,
) {
  const managed = plugin.managed_server;
  const fields = plugin.settings.filter(
    (field) =>
      !field.hidden &&
      !(
        isManagedMode(plugin, settings) &&
        managed &&
        field.name in managed.bindings
      ),
  );
  return [
    ...fields.filter((field) => field.name === managed?.mode_setting),
    ...fields.filter((field) => field.name !== managed?.mode_setting),
  ];
}

export function pluginSettingsPayload(
  plugin: Plugin,
  settings: Record<string, unknown>,
  editedSecrets: Set<string>,
) {
  return Object.fromEntries(
    pluginFields(plugin, settings)
      .filter(
        (field) =>
          !(field.type === "secret" || field.secret) ||
          editedSecrets.has(field.name),
      )
      .filter((field) => settings[field.name] !== undefined)
      .map((field) => [field.name, settings[field.name]]),
  );
}

export function managedPhaseLabel(
  phase: Schemas["ManagedServerStatus"]["phase"],
) {
  return {
    stopped: "",
    preparing: "Downloading and preparing",
    starting: "Starting",
    ready: "Ready",
    failed: "Couldn't start",
  }[phase];
}

export function managedPluginError(
  error: unknown,
  plugin: Plugin,
  game?: Game,
) {
  if (!error) return null;
  const code =
    typeof error === "object" && "code" in error ? error.code : undefined;
  if (code === "gateway_unconfigured") {
    return {
      message:
        "This Varde server can't publish addresses yet. Ask the operator to set up the gateway.",
      needsPC: false,
    };
  }
  if (code === "group_pc_required") {
    const details =
      typeof error === "object" &&
      error &&
      "details" in error &&
      error.details &&
      typeof error.details === "object"
        ? (error.details as Record<string, unknown>)
        : {};
    const required = details.os ?? details.required_os ?? game?.os;
    const labels: Record<string, string> = {
      linux: "Linux",
      windows: "Windows",
      darwin: "macOS",
    };
    const names = (
      Array.isArray(required)
        ? required
        : typeof required === "string"
          ? [required]
          : []
    )
      .filter((os): os is string => typeof os === "string")
      .map((os) => labels[os] ?? os);
    return {
      message: `${plugin.name} needs a ${names.length ? names.join(" or ") : "compatible"} PC in your group.`,
      needsPC: true,
    };
  }
  return { message: errorMessage(error), needsPC: false };
}

export function managedPublicURL(value?: string) {
  try {
    const url = new URL(value ?? "");
    return url.protocol === "https:" || url.protocol === "http:"
      ? url.href
      : undefined;
  } catch {
    return undefined;
  }
}
