import type { Node } from "../api/client";

export type Cs2Mode = {
  id: string;
  name: string;
  blurb: string;
  // Valve's player limit for the mode (gamemodes.txt maxplayers)
  slots: number;
  maps: readonly string[];
};

const CLASSIC_MAPS = [
  "de_dust2",
  "de_mirage",
  "de_inferno",
  "de_nuke",
  "de_overpass",
  "de_train",
  "de_vertigo",
  "de_ancient",
  "de_anubis",
  "cs_office",
  "cs_italy",
] as const;

// Valve's mg_skirmish_retakes map group
const RETAKE_MAPS = [
  "de_dust2",
  "de_mirage",
  "de_inferno",
  "de_nuke",
  "de_overpass",
  "de_train",
  "de_vertigo",
  "de_ancient",
] as const;

const ARMS_RACE_MAPS = ["ar_baggage", "ar_shoots", "ar_pool_day"] as const;

export const CS2_MODES: readonly Cs2Mode[] = [
  {
    id: "casual",
    name: "Casual",
    blurb: "Relaxed bomb and hostage rounds. Drop in and out any time.",
    slots: 20,
    maps: CLASSIC_MAPS,
  },
  {
    id: "competitive",
    name: "Competitive",
    blurb: "Five against five with money and buy rounds, like matchmaking.",
    slots: 10,
    maps: CLASSIC_MAPS,
  },
  {
    id: "wingman",
    name: "Wingman",
    blurb: "Two against two. Short, tense matches for a small group.",
    slots: 4,
    maps: CLASSIC_MAPS,
  },
  {
    id: "retakes",
    name: "Retakes",
    blurb:
      "The bomb is already planted. Terrorists hold the site, CTs take it back.",
    slots: 7,
    maps: RETAKE_MAPS,
  },
  {
    id: "arms_race",
    name: "Arms Race",
    blurb: "Respawn instantly and get a new weapon with every kill.",
    slots: 16,
    maps: ARMS_RACE_MAPS,
  },
  {
    id: "deathmatch",
    name: "Deathmatch",
    blurb: "Respawn instantly and warm up your aim. No rounds.",
    slots: 16,
    maps: CLASSIC_MAPS,
  },
];

const MAP_NAMES: Record<string, string> = {
  de_dust2: "Dust II",
  de_mirage: "Mirage",
  de_inferno: "Inferno",
  de_nuke: "Nuke",
  de_overpass: "Overpass",
  de_train: "Train",
  de_vertigo: "Vertigo",
  de_ancient: "Ancient",
  de_anubis: "Anubis",
  cs_office: "Office",
  cs_italy: "Italy",
  ar_baggage: "Baggage",
  ar_shoots: "Shoots",
  ar_pool_day: "Pool Day",
};

export function mapName(id: string) {
  return MAP_NAMES[id] ?? id;
}

/** Modes the catalog offers, in the order players know them. */
export function cs2Modes(options: readonly string[] | undefined) {
  const offered = new Set(options ?? []);
  return CS2_MODES.filter((m) => offered.has(m.id));
}

/** Config changes that keep a mode switch playable. */
export function applyCs2Mode(
  config: Record<string, unknown>,
  mode: Cs2Mode,
  keepSlots: boolean,
): Record<string, unknown> {
  const next: Record<string, unknown> = { ...config, game_mode: mode.id };
  if (!keepSlots) next.max_players = mode.slots;
  if (typeof next.map !== "string" || !mode.maps.includes(next.map))
    next.map = mode.maps[0];
  return next;
}

export const CS2_ADVANCED = ["password", "max_players", "bots", "gslt"];

/** What starting a Steam game on this host involves, when the host says. */
export function hostReadiness(
  n: Node,
  steamApp: number | undefined,
  minDiskMb: number | undefined,
  minMemoryMb: number,
): { ok: boolean; installed?: boolean; note: string } | null {
  const caps = n.capabilities;
  if (!caps) return null;
  if (caps.memory_total_mb && caps.memory_total_mb < minMemoryMb)
    return {
      ok: false,
      note: `Has ${Math.round(caps.memory_total_mb / 1024)} GB RAM; needs ${Math.round(minMemoryMb / 1024)} GB`,
    };
  if (steamApp && caps.steam_apps?.includes(steamApp))
    return { ok: true, installed: true, note: "Game already installed" };
  if (minDiskMb && caps.disk_free_bytes !== undefined) {
    const freeGb = caps.disk_free_bytes / 1e9;
    const needGb = minDiskMb / 1000;
    if (freeGb < needGb)
      return {
        ok: false,
        note: `${Math.floor(freeGb)} GB free; downloading needs ${Math.round(needGb)} GB`,
      };
    return { ok: true, note: "Downloads the game server on first start" };
  }
  return null;
}
