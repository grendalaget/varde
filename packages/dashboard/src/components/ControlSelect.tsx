import { Description, Label, ListBox, Select } from "@heroui/react";
import type { ReactNode } from "react";

type Option<T extends string> = { value: T; label: string };

export function ControlSelect<T extends string>({
  label,
  showLabel,
  help,
  className,
  triggerClassName,
  trigger,
  options,
  value,
  onChange,
  disabled,
}: {
  label: string;
  showLabel?: boolean;
  help?: string;
  className?: string;
  triggerClassName?: string;
  trigger?: (value: ReactNode) => ReactNode;
  options: readonly Option<T>[];
  value: string;
  onChange: (value: T) => void;
  disabled?: boolean;
}) {
  const selectedValue = (
    <Select.Value className="min-w-0 truncate text-start" />
  );
  return (
    <Select
      className={className}
      isDisabled={disabled}
      selectedKey={`option:${value}`}
      onSelectionChange={(key) => {
        const option = options.find((item) => `option:${item.value}` === key);
        if (option) onChange(option.value);
      }}
    >
      <Label
        className={
          showLabel ? "text-sm font-medium text-foreground" : "sr-only"
        }
      >
        {label}
      </Label>
      <Select.Trigger className={triggerClassName ?? "py-1.5"}>
        {trigger ? trigger(selectedValue) : selectedValue}
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover className="vd-choice-popover keep-motion" offset={5}>
        <ListBox>
          {options.map((option) => (
            <ListBox.Item
              key={option.value}
              id={`option:${option.value}`}
              textValue={option.label}
            >
              <Label>{option.label}</Label>
              <ListBox.ItemIndicator />
            </ListBox.Item>
          ))}
        </ListBox>
      </Select.Popover>
      {help && <Description className="text-xs text-muted">{help}</Description>}
    </Select>
  );
}
