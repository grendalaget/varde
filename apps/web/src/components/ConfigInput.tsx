import { Checkbox } from "@heroui/react";
import type { GameConfigField, Schemas } from "../api/client";
import { Field, inputClass } from "./ui";

type ConfigField = GameConfigField | Schemas["PluginField"];

export function ConfigInput({
  f,
  value,
  onChange,
  members = [],
  disabled = false,
}: {
  f: ConfigField;
  value: unknown;
  onChange: (v: unknown) => void;
  members?: Schemas["Member"][];
  disabled?: boolean;
}) {
  const label = f.name
    .replace(/_/g, " ")
    .replace(/^\w/, (c) => c.toUpperCase());
  if (f.type === "bool") {
    return (
      <div className="flex items-start gap-3">
        <Checkbox
          isSelected={!!value}
          isDisabled={disabled}
          onChange={(selected) => onChange(selected)}
        >
          <span className="text-sm text-foreground">{label}</span>
        </Checkbox>
        {f.help && <span className="text-xs text-muted">{f.help}</span>}
      </div>
    );
  }
  if (f.type === "multiselect") {
    const selected = Array.isArray(value) ? value.map(String) : [];
    return (
      <Field label={label} help={f.help}>
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
      <Field label={label} help={f.help}>
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
                className="w-full rounded-md px-2 py-1 hover:bg-default/30"
              >
                <span className="flex w-full items-center justify-between gap-2 text-sm">
                  <span>{member.display_name}</span>
                  {disabled && <span className="text-xs text-muted">no Steam ID</span>}
                </span>
              </Checkbox>
            );
          })}
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
