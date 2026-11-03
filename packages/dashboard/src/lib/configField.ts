import type { GameConfigField, Schemas } from "../api/client";

type ConfigField = GameConfigField | Schemas["PluginField"];

export function initialFieldValue(f: ConfigField, stored?: unknown): unknown {
  if (f.type === "secret" || f.secret) return "";
  if (stored !== undefined && stored !== null) return stored;
  if (f.default !== undefined && f.default !== null) return f.default;
  switch (f.type) {
    case "bool":
      return false;
    case "multiselect":
    case "member_multiselect":
      return [];
    case "int":
      return undefined;
    default:
      return "";
  }
}

export function updateFieldValue(
  current: Record<string, unknown>,
  name: string,
  value: unknown,
): Record<string, unknown> {
  const next = { ...current };
  if (value === undefined) delete next[name];
  else next[name] = value;
  return next;
}

/** Fields that read better across both columns of a two-column form. */
export function isWideField(f: ConfigField) {
  return (
    f.type === "bool" ||
    f.type === "multiselect" ||
    f.type === "member_multiselect" ||
    (f.help?.length ?? 0) > 50
  );
}
