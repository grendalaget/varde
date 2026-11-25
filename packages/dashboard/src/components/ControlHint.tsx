import { Tooltip } from "@heroui/react";
import type { ButtonHTMLAttributes, ReactNode } from "react";

export function ControlHint({
  text,
  children,
}: {
  text: string;
  children: ReactNode;
}) {
  return (
    <Tooltip delay={350}>
      {children}
      <Tooltip.Content className="vd-control-hint keep-motion" offset={6}>
        {text}
      </Tooltip.Content>
    </Tooltip>
  );
}

export function HintButton({
  hint,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { hint: string }) {
  return (
    <ControlHint text={hint}>
      <Tooltip.Trigger<"button">
        render={(props) => <button {...props} />}
        {...props}
      />
    </ControlHint>
  );
}

export function HintText({
  hint,
  children,
  className,
  dateTime,
}: {
  hint: string;
  children: ReactNode;
  className?: string;
  dateTime?: string;
}) {
  return (
    <ControlHint text={hint}>
      {dateTime ? (
        <Tooltip.Trigger<"time">
          render={(props) => (
            <time {...props} role={undefined} dateTime={dateTime} />
          )}
          className={className}
        >
          {children}
        </Tooltip.Trigger>
      ) : (
        <Tooltip.Trigger<"span">
          render={(props) => <span {...props} role={undefined} />}
          className={className}
        >
          {children}
        </Tooltip.Trigger>
      )}
    </ControlHint>
  );
}
