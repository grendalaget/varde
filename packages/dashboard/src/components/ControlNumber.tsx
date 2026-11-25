import { useId, useRef } from "react";
import { cx, inputClass } from "./ui";

export function ControlNumber({
  label,
  help,
  value,
  onChange,
  min,
  required,
  disabled,
}: {
  label: string;
  help?: string;
  value: string | number;
  onChange: (value: string) => void;
  min?: number;
  required?: boolean;
  disabled?: boolean;
}) {
  const id = useId();
  const input = useRef<HTMLInputElement>(null);

  function step(direction: "up" | "down") {
    const field = input.current;
    if (!field) return;
    if (direction === "up") field.stepUp();
    else field.stepDown();
    field.focus();
    onChange(field.value);
  }

  return (
    <div>
      <label htmlFor={id} className="block text-sm font-medium text-foreground">
        {label}
      </label>
      <div className="relative mt-1">
        <input
          ref={input}
          id={id}
          type="number"
          className={cx(inputClass, "vd-number-input pe-[4.25rem]")}
          aria-describedby={help ? `${id}-help` : undefined}
          required={required}
          disabled={disabled}
          min={min}
          value={value}
          onChange={(event) => onChange(event.target.value)}
        />
        <div className="absolute inset-y-0 end-1.5 flex items-center gap-0.5">
          {(["down", "up"] as const).map((direction) => (
            <button
              key={direction}
              type="button"
              tabIndex={-1}
              aria-label={`${direction === "up" ? "Increase" : "Decrease"} ${label}`}
              disabled={disabled}
              onClick={() => step(direction)}
              className="grid size-7 place-items-center rounded-full text-muted outline-focus transition-colors hover:bg-default hover:text-foreground focus-visible:outline-2 disabled:opacity-40"
            >
              {direction === "up" ? "+" : "−"}
            </button>
          ))}
        </div>
      </div>
      {help && (
        <p id={`${id}-help`} className="mt-1 text-xs text-muted">
          {help}
        </p>
      )}
    </div>
  );
}
