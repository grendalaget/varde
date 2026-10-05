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
