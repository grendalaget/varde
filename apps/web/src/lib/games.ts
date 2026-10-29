import { api, type Game } from "../api/client";
import { useLoad } from "./data";

let gamesCache: Game[] | null = null;

export function useGames() {
  const { data } = useLoad(async () => {
    if (gamesCache) return { data: gamesCache };
    const r = await api.GET("/v1/games");
    if (r.data) gamesCache = r.data.games;
    return { data: r.data?.games, error: r.error };
  }, []);
  const games = data ?? [];
  return {
    games,
    get: (id: string) => games.find((g) => g.id === id),
    name: (id: string) => games.find((g) => g.id === id)?.name ?? id,
  };
}

export function isCrossplayEnabled(
  game: Game | undefined,
  config: Record<string, unknown>,
): boolean {
  return (
    config.crossplay === true &&
    game?.config_fields.some((field) => field.name === "crossplay") === true
  );
}

// A game's server_name (its server-browser title) follows the Varde name,
// so the forms ask for a name once.
const NAME_FIELD = "server_name";

export function askedFields(game: Game | undefined) {
  return game?.config_fields.filter((f) => f.name !== NAME_FIELD) ?? [];
}

export function withServerName(
  game: Game | undefined,
  config: Record<string, unknown>,
  name: string,
): Record<string, unknown> {
  return game?.config_fields.some((f) => f.name === NAME_FIELD)
    ? { ...config, [NAME_FIELD]: name }
    : config;
}

// Tile colour and monogram per game; unknown games get a neutral stone.
const ART: Record<string, { tint: string; mark: string }> = {
  cs2: { tint: "#f0a23b", mark: "CS" },
  minecraft: { tint: "#5fb04f", mark: "MC" },
  valheim: { tint: "#6c9bd4", mark: "V" },
};

export function gameArt(id?: string, name?: string) {
  return (
    ART[id ?? ""] ?? {
      tint: "#958d8b",
      mark: (name ?? id ?? "").slice(0, 1).toUpperCase(),
    }
  );
}

/** Banner background washed in the game's colour. */
export function gameBackdrop(id?: string) {
  const { tint } = gameArt(id);
  return `radial-gradient(120% 140% at 100% 0%, color-mix(in oklab, ${tint} 40%, transparent), transparent 60%), radial-gradient(90% 120% at 0% 100%, color-mix(in oklab, ${tint} 16%, transparent), transparent 70%)`;
}
