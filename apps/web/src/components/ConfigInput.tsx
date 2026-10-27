import { Checkbox } from "@heroui/react";
import type { GameConfigField, Schemas } from "../api/client";
import { Link } from "../lib/router";
import { Field, inputClass, Toggle } from "./ui";

type ConfigField = GameConfigField | Schemas["PluginField"];

export function ConfigInput({
  f,
  value,
  onChange,
  members = [],
  disabled = false,
  showSteamHint = false,
}: {
  f: ConfigField;
  value: unknown;
  onChange: (v: unknown) => void;
  members?: Schemas["Member"][];
  disabled?: boolean;
  showSteamHint?: boolean;
}) {
  const fallbackLabel = f.name
    .replace(/_/g, " ")
    .replace(/^\w/, (c) => c.toUpperCase());
  const label = ("label" in f ? f.label?.trim() : "") || fallbackLabel;
  const placeholder = "placeholder" in f ? f.placeholder : undefined;
  if (f.type === "bool") {
    return (
      <Toggle checked={!!value} disabled={disabled} onChange={onChange} label={label}>
        <span className="text-sm text-foreground">{label}</span>
        {f.help && <span className="text-xs text-muted">{f.help}</span>}
      </Toggle>
    );
  }
  if (f.type === "multiselect") {
    const selected = Array.isArray(value) ? value.map(String) : [];
    return (
      <Field group label={label} help={f.help}>
        <div className="flex flex-wrap gap-2">
          {(f.options ?? []).map((option) => {
            const choice = String(option);
            const active = selected.includes(choice);
            return (
              <button
                key={choice}
                type="button"
                aria-pressed={active}
                disabled={disabled}
                onClick={() =>
                  onChange(
                    active
                      ? selected.filter((item) => item !== choice)
                      : [...selected, choice],
                  )
                }
                className={
                  "rounded-full border px-3 py-1.5 text-sm transition-colors " +
                  (active
                    ? "border-foreground bg-default/40 text-foreground"
                    : "border-field-border text-muted hover:text-foreground")
                }
              >
                {choice}
              </button>
            );
          })}
        </div>
      </Field>
    );
  }
  if (f.type === "member_multiselect") {
    const selected = Array.isArray(value) ? value.map(String) : [];
    const requiresSteam = f.requires === "steam_id";
    return (
      <Field group label={label} help={f.help}>
        <div className="space-y-1">
          {members.map((member) => {
            const memberDisabled = disabled || (requiresSteam && !member.steam_id);
            return (
              <Checkbox
                key={member.user_id}
                isSelected={selected.includes(member.user_id)}
                isDisabled={memberDisabled}
                onChange={(checked: boolean) =>
                  onChange(
                    checked
                      ? [...selected, member.user_id]
                      : selected.filter((id) => id !== member.user_id),
                  )
                }
                variant="primary"
                className="w-full rounded-md px-2 py-1 hover:bg-default/30"
              >
                <Checkbox.Content className="flex w-full items-center gap-2 text-sm">
                  <Checkbox.Control>
                    <Checkbox.Indicator />
                  </Checkbox.Control>
                  <span className="min-w-0 flex-1 truncate">{member.display_name}</span>
                  {requiresSteam && !member.steam_id && (
                    <span className="text-xs text-muted">no Steam ID</span>
                  )}
                </Checkbox.Content>
              </Checkbox>
            );
          })}
          {showSteamHint && requiresSteam && (
            <p className="pt-1 text-xs text-muted">
              Members without a Steam ID can link one in{" "}
              <Link
                to="/settings"
                className="text-foreground underline decoration-muted underline-offset-2 hover:decoration-foreground"
              >
                Settings
              </Link>
              .
            </p>
          )}
        </div>
      </Field>
    );
  }
  return (
    <Field label={label} help={f.help}>
      {f.type === "select" ? (
        <select
          className={inputClass}
          disabled={disabled}
          value={String(value ?? "")}
          onChange={(e) => onChange(e.target.value)}
        >
          {(f.options ?? []).map((o) => (
            <option key={String(o)} value={String(o)}>
              {String(o)}
            </option>
          ))}
        </select>
      ) : (
        <input
          className={inputClass}
          type={
            f.type === "int"
              ? "number"
              : f.type === "secret" || f.secret
                ? "password"
                : "text"
          }
          required={f.required}
          disabled={disabled}
          value={value == null ? "" : String(value)}
          placeholder={
            f.type === "string" || f.type === "secret" || f.secret
              ? placeholder
              : undefined
          }
          onChange={(e) =>
            onChange(
              f.type === "int"
                ? e.target.value === ""
                  ? undefined
                  : Number(e.target.value)
                : e.target.value,
            )
          }
        />
      )}
    </Field>
  );
}
