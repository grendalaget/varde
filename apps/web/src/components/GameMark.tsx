import { gameArt } from "../lib/games";
import { cx } from "./ui";

const SIZES = {
  sm: "size-9 rounded-lg text-xs",
  md: "size-11 rounded-xl text-sm",
  lg: "size-16 rounded-2xl text-xl",
};

export function GameMark({
  id,
  name,
  size = "md",
}: {
  id?: string;
  name?: string;
  size?: keyof typeof SIZES;
}) {
  const { tint, mark } = gameArt(id, name);
  return (
    <span
      aria-hidden
      className={cx(
        "grid shrink-0 place-items-center font-bold tracking-tight text-natt shadow-[inset_0_1px_0_rgb(255_255_255/0.35),0_6px_18px_-6px_rgb(0_0_0/0.6)]",
        SIZES[size],
      )}
      style={{
        background: `linear-gradient(145deg, color-mix(in oklab, ${tint} 80%, white), color-mix(in oklab, ${tint} 75%, black))`,
      }}
    >
      {mark}
    </span>
  );
}

/** Soft corner glow in the game's colour, for cards and banners. */
export function GameGlow({
  id,
  className,
}: {
  id?: string;
  className?: string;
}) {
  return (
    <span
      aria-hidden
      className={cx(
        "pointer-events-none absolute -top-20 -right-12 size-48 rounded-full blur-3xl transition-opacity duration-300",
        className,
      )}
      style={{ background: gameArt(id).tint }}
    />
  );
}
