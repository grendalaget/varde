import type { GameConfigField } from "../api/client";

export function configDefaults(
  fields: Pick<GameConfigField, "name" | "default">[],
): Record<string, unknown> {
  const config: Record<string, unknown> = {};
  for (const field of fields)
    if (field.default != null) config[field.name] = field.default;
  return config;
}
