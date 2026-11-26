import {
  useEffect,
  useMemo,
  useState,
  type CSSProperties,
  type ReactNode,
} from "react";
import { api, errorMessage, type Game } from "../api/client";
import {
  Button,
  Callout,
  ChoiceCard,
  cx,
  Dot,
  ErrorNote,
  inputClass,
  PageHeader,
  Toggle,
} from "../components/ui";
import { GameMark } from "../components/GameMark";
import { navigate, useLocation } from "../lib/router";
import { useSession, type MeGroup } from "../lib/session";
import {
  askedFields,
  gameArt,
  gameBackdrop,
  useGames,
  withServerName,
} from "../lib/games";
import { configDefaults } from "../lib/config-defaults";
import { ConfigInput } from "../components/ConfigInput";
import { ControlSelect } from "../components/ControlSelect";
import { isWideField } from "../lib/configField";
import {
  applyCs2Mode,
  CS2_ADVANCED,
  cs2Modes,
  hostReadiness,
  mapName,
} from "../lib/cs2";

function needs(g?: Game) {
  if (!g) return "";
  const ram = `${Math.round((g.min_memory_mb / 1024) * 10) / 10} GB RAM`;
  // min_disk_mb is decimal MB (the scheduler counts 1 MB = 10^6 bytes)
  return g.min_disk_mb
    ? `${ram} · ${Math.round(g.min_disk_mb / 1000)} GB disk`
    : ram;
}

export default function NewServer() {
  const { me, group, nodes, nodesLoaded, selectGroup } = useSession();
  const { games } = useGames();
  const { search } = useLocation();
  const wanted = search.get("game");
  const [gameId, setGameId] = useState("");
  const [name, setName] = useState("");
  const [config, setConfig] = useState<Record<string, unknown>>({});
  const [preferred, setPreferred] = useState("");
  const [startNow, setStartNow] = useState(true);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [createdId, setCreatedId] = useState<string | null>(null);
  const [slotsTouched, setSlotsTouched] = useState(false);

  // games first; web tools (they get a gateway URL, like Auto Tournament) last
  const visibleGames = games
    .filter((g) => g.id !== "testgame" || import.meta.env.DEV)
    .sort((a, b) => Number(!!a.gateway_port) - Number(!!b.gateway_port));
  const game = useMemo(
    () => games.find((g) => g.id === gameId),
    [games, gameId],
  );
  const fields = askedFields(game);
  const namesInGame = !!game && fields.length < game.config_fields.length;
  const cs2 = game?.id === "cs2";

  // keep the game the player came for (?game=cs2); otherwise let them pick
  useEffect(() => {
    if (!gameId && wanted && visibleGames.some((g) => g.id === wanted))
      setGameId(wanted);
  }, [gameId, wanted, visibleGames]);

  useEffect(() => {
    if (!game) return;
    setConfig(configDefaults(game.config_fields));
    setSlotsTouched(false);
  }, [game]);

  const hosts = nodes.filter(
    (n) => n.hosting_enabled && n.admin_state === "active",
  );
  // nothing could host a start: say so up front instead of failing after create
  const canStart = !nodesLoaded || hosts.length > 0;
  const starting = startNow && canStart;
  const host = hosts.find((n) => n.id === preferred);
  const readiness = (n: (typeof hosts)[number]) =>
    game
      ? hostReadiness(n, game.steam_app, game.min_disk_mb, game.min_memory_mb)
      : null;
  const hostNote = host ? readiness(host) : null;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    const { data, error } = await api.POST("/v1/groups/{groupId}/servers", {
      params: { path: { groupId: group.group_id } },
      body: {
        name,
        game_id: gameId,
        config: withServerName(game, config, name),
        preferred_node_id: preferred || undefined,
      },
    });
    if (error || !data) {
      setBusy(false);
      setErr(errorMessage(error));
      return;
    }
    if (starting) {
      const { error: startError } = await api.POST(
        "/v1/servers/{serverId}/start",
        {
          params: { path: { serverId: data.id } },
          body: {},
        },
      );
      if (startError) {
        setBusy(false);
        // the server exists — offer "Open server" alongside the error
        setCreatedId(data.id);
        setErr(
          `The server was created, but it didn't start. ${errorMessage(startError)}`,
        );
        return;
      }
    }
    navigate(`/servers/${data.id}`);
  }

  return (
    <>
      <PageHeader
        title="New server"
        subtitle="Pick a game and give it a name. Varde runs it on one of your group's PCs."
      />
      {nodesLoaded && nodes.length === 0 && (
        <NoMachines
          group={group}
          others={me.groups.filter((g) => g.group_id !== group.group_id)}
          onSwitch={selectGroup}
        />
      )}
      <form
        onSubmit={submit}
        className="grid items-start gap-8 lg:grid-cols-[minmax(0,1fr)_19rem]"
      >
        <div className="vd-stagger min-w-0 space-y-10">
          <Section
            n={1}
            title="What do you want to play?"
            hint={game ? undefined : "Pick a game to see its settings."}
          >
            <div
              role="radiogroup"
              aria-label="Game"
              className="grid gap-3 sm:grid-cols-2"
            >
              {visibleGames.map((g) => (
                <GameTile
                  key={g.id}
                  g={g}
                  selected={g.id === gameId}
                  onSelect={() => setGameId(g.id)}
                />
              ))}
            </div>
          </Section>

          <Section
            n={2}
            title="Name"
            hint={
              namesInGame
                ? "Shown in Varde and in the game's server list."
                : "Shown in Varde."
            }
          >
            <input
              className={cx(inputClass, "px-4 py-2.5 text-base")}
              required
              aria-label="Server name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. Friday night server"
            />
          </Section>

          {cs2 && game && (
            <Cs2Settings
              game={game}
              config={config}
              onChange={setConfig}
              slotsTouched={slotsTouched}
              onSlotsTouched={() => setSlotsTouched(true)}
            />
          )}

          {!cs2 && fields.length > 0 && (
            <Section n={3} title="Settings" hint="You can change these later.">
              <div className="grid gap-x-5 gap-y-5 sm:grid-cols-2">
                {fields.map((f) => (
                  <div
                    key={f.name}
                    className={isWideField(f) ? "sm:col-span-2" : undefined}
                  >
                    <ConfigInput
                      f={f}
                      value={config[f.name]}
                      onChange={(v) =>
                        setConfig((c) => ({ ...c, [f.name]: v }))
                      }
                    />
                  </div>
                ))}
              </div>
            </Section>
          )}

          <Section
            n={cs2 ? 5 : fields.length > 0 ? 4 : 3}
            title="Machine"
            hint="Varde prefers this machine when it's online, and uses another one otherwise."
          >
            <div
              role="radiogroup"
              aria-label="Preferred machine"
              className="grid gap-2.5 sm:grid-cols-2"
            >
              <ChoiceCard
                selected={preferred === ""}
                onSelect={() => setPreferred("")}
                title="Any machine"
                sub="Recommended: Varde picks one that can run it"
              />
              {hosts.map((n) => {
                const r = readiness(n);
                return (
                  <ChoiceCard
                    key={n.id}
                    selected={preferred === n.id}
                    onSelect={() => setPreferred(n.id)}
                    title={n.name}
                    sub={
                      <>
                        <Dot tone={n.online ? "green" : "neutral"} />
                        {n.online ? "Online" : "Offline"}
                        {r && (
                          <span className={r.ok ? undefined : "text-warning"}>
                            {" "}
                            · {r.note}
                          </span>
                        )}
                      </>
                    }
                  />
                );
              })}
            </div>
          </Section>
        </div>

        <aside className="vd-enter lg:sticky lg:top-8">
          <div className="overflow-hidden rounded-2xl border border-separator bg-surface shadow-surface">
            <Banner g={game} />
            <div className="space-y-5 p-5">
              <div className="min-w-0">
                <div className="truncate text-xs font-medium uppercase tracking-wider text-muted">
                  {game?.name ?? "No game picked"}
                </div>
                <div className="mt-1 truncate text-lg font-semibold text-foreground">
                  {name.trim() || (
                    <span className="text-muted/70">Untitled server</span>
                  )}
                </div>
              </div>
              <dl className="space-y-2 text-sm">
                <Row label="Runs on" value={host?.name ?? "Any machine"} />
                <Row label="Needs" value={needs(game)} />
              </dl>
              {game?.steam_app && (
                <p className="text-xs text-muted">
                  {hostNote?.installed
                    ? `${game.name} is already installed on ${host?.name}, so nothing extra downloads.`
                    : "If the game is installed through Steam on the host PC, Varde uses it. Otherwise the first start downloads the dedicated server, which can take a while."}
                </p>
              )}
              <div className="border-t border-separator pt-4">
                <Toggle
                  checked={starting}
                  disabled={!canStart}
                  onChange={setStartNow}
                  label="Start now"
                >
                  <span className="text-sm text-foreground">
                    Start it right away
                  </span>
                </Toggle>
                {!canStart && (
                  <p className="mt-1.5 text-xs text-muted">
                    Add a machine to the group to start it.
                  </p>
                )}
                {canStart && !starting && (
                  <p className="mt-1.5 text-xs text-muted">
                    You can start it later from the server page.
                  </p>
                )}
              </div>
              <ErrorNote>{err}</ErrorNote>
              <div className="flex flex-col gap-2">
                {createdId ? (
                  <Button
                    type="button"
                    variant="primary"
                    className="w-full"
                    onClick={() => navigate(`/servers/${createdId}`)}
                  >
                    Open server
                  </Button>
                ) : (
                  <Button
                    type="submit"
                    variant="primary"
                    className="w-full"
                    busy={busy}
                    disabled={!gameId}
                  >
                    {starting ? "Create and start" : "Create server"}
                  </Button>
                )}
                <Button
                  type="button"
                  variant="ghost"
                  className="w-full"
                  onClick={() => navigate("/")}
                >
                  Cancel
                </Button>
              </div>
            </div>
          </div>
        </aside>
      </form>
    </>
  );
}

const WORKSHOP = "__workshop";

function Cs2Settings({
  game,
  config,
  onChange,
  slotsTouched,
  onSlotsTouched,
}: {
  game: Game;
  config: Record<string, unknown>;
  onChange: (c: Record<string, unknown>) => void;
  slotsTouched: boolean;
  onSlotsTouched: () => void;
}) {
  const modes = cs2Modes(
    game.config_fields.find((f) => f.name === "game_mode")?.options,
  );
  const mode = modes.find((m) => m.id === config.game_mode) ?? modes[0];
  const map = typeof config.map === "string" ? config.map : "";
  const workshopField = game.config_fields.some((f) => f.name === "workshop_map");
  const workshopMap =
    typeof config.workshop_map === "string" ? config.workshop_map : "";
  const [workshop, setWorkshop] = useState(workshopMap !== "");
  const otherMap = !workshop && !!mode && !mode.maps.includes(map);
  const advanced = game.config_fields.filter((f) =>
    CS2_ADVANCED.includes(f.name),
  );
  return (
    <>
      <Section n={3} title="Mode" hint="You can change these later.">
        <div
          role="radiogroup"
          aria-label="Mode"
          className="grid gap-2.5 sm:grid-cols-2"
        >
          {modes.map((m) => (
            <ChoiceCard
              key={m.id}
              selected={m.id === mode?.id}
              onSelect={() => onChange(applyCs2Mode(config, m, slotsTouched))}
              title={m.name}
              sub={m.blurb}
            />
          ))}
        </div>
      </Section>

      {mode && (
        <Section n={4} title="Map">
          <div className="grid gap-3 sm:grid-cols-2">
            <ControlSelect
              label="Map"
              value={workshop ? WORKSHOP : otherMap ? "" : map}
              options={[
                ...mode.maps.map((id) => ({ value: id, label: mapName(id) })),
                { value: "", label: "Another map…" },
                ...(workshopField
                  ? [{ value: WORKSHOP, label: "Workshop map…" }]
                  : []),
              ]}
              onChange={(next) => {
                setWorkshop(next === WORKSHOP);
                if (next === WORKSHOP) return;
                const rest: typeof config = { ...config, map: next };
                delete rest.workshop_map;
                onChange(rest);
              }}
            />
            {workshop && (
              <input
                className={cx(inputClass, "font-mono")}
                aria-label="Workshop map link or ID"
                required
                value={workshopMap}
                onChange={(e) =>
                  onChange({ ...config, workshop_map: e.target.value })
                }
                placeholder="Workshop link or item ID"
              />
            )}
            {otherMap && (
              <input
                className={cx(inputClass, "font-mono")}
                aria-label="Map name"
                required
                value={map}
                onChange={(e) => onChange({ ...config, map: e.target.value })}
                placeholder="e.g. de_cache"
              />
            )}
          </div>
          {workshop && (
            <p className="mt-2 text-sm text-muted">
              The host downloads the map from Steam before the server starts,
              so the first start takes longer. Players download it from Steam
              when they join.
            </p>
          )}
        </Section>
      )}

      {advanced.length > 0 && (
        <details
          className="vd-disclosure group rounded-2xl border border-separator bg-surface sm:ml-9"
          onInvalidCapture={(event) => {
            const disclosure = event.currentTarget;
            if (disclosure.open) return;
            disclosure.dataset.validationOpen = "";
            disclosure.open = true;
            requestAnimationFrame(() => delete disclosure.dataset.validationOpen);
          }}
        >
          <summary className="cursor-pointer list-none rounded-2xl px-4 py-3 text-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent">
            <span className="font-medium text-foreground">Advanced</span>
            <span className="text-muted">
              {" "}
              · password, player slots, bots, Steam token
            </span>
          </summary>
          <div className="space-y-4 border-t border-separator p-4">
            <p className="text-sm text-muted">
              Most game nights don&apos;t need these. Player slots follow the
              mode unless you change them here.
            </p>
            <div className="grid gap-x-5 gap-y-5 sm:grid-cols-2">
              {advanced.map((f) => (
                <div
                  key={f.name}
                  className={isWideField(f) ? "sm:col-span-2" : undefined}
                >
                  <ConfigInput
                    f={f}
                    value={config[f.name]}
                    onChange={(v) => {
                      if (f.name === "max_players") onSlotsTouched();
                      onChange({ ...config, [f.name]: v });
                    }}
                  />
                </div>
              ))}
            </div>
          </div>
        </details>
      )}
    </>
  );
}

function Section({
  n,
  title,
  hint,
  children,
}: {
  n: number;
  title: string;
  hint?: string;
  children: ReactNode;
}) {
  return (
    <section>
      <div className="mb-4 flex items-start gap-3">
        <span className="mt-px grid size-6 shrink-0 place-items-center rounded-full bg-default text-xs font-semibold tabular-nums text-muted ring-1 ring-inset ring-border">
          {n}
        </span>
        <div>
          <h2 className="text-base font-semibold leading-6 text-foreground">
            {title}
          </h2>
          {hint && <p className="text-sm text-muted">{hint}</p>}
        </div>
      </div>
      <div className="sm:pl-9">{children}</div>
    </section>
  );
}

function GameTile({
  g,
  selected,
  onSelect,
}: {
  g: Game;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      onClick={onSelect}
      style={{ "--tint": gameArt(g.id).tint } as CSSProperties}
      className={cx(
        "group relative overflow-hidden rounded-2xl border bg-surface p-4 text-left transition-all duration-200 ease-vd-out hover:-translate-y-0.5 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent active:scale-[0.99]",
        selected
          ? "border-(--tint) shadow-[0_0_0_1px_var(--tint),0_16px_40px_-20px_var(--tint)]"
          : "border-separator hover:border-field-border",
      )}
    >
      <span
        aria-hidden
        className={cx(
          "pointer-events-none absolute -top-20 -right-12 size-48 rounded-full bg-(--tint) blur-3xl transition-opacity duration-300",
          selected ? "opacity-30" : "opacity-[0.08] group-hover:opacity-15",
        )}
      />
      <span className="relative flex items-center gap-3.5">
        <GameMark id={g.id} name={g.name} />
        <span className="min-w-0 flex-1">
          <span className="block font-semibold leading-snug text-foreground">
            {g.name}
          </span>
          <span className="mt-0.5 block text-xs text-muted">{needs(g)}</span>
        </span>
        <Check on={selected} />
      </span>
    </button>
  );
}

function Check({ on }: { on: boolean }) {
  return (
    <span
      aria-hidden
      className={cx(
        "grid size-5 shrink-0 place-items-center rounded-full bg-(--tint) text-natt transition-all duration-300 ease-vd-spring",
        on ? "scale-100 opacity-100" : "scale-50 opacity-0",
      )}
    >
      <svg
        viewBox="0 0 24 24"
        className="size-3"
        fill="none"
        stroke="currentColor"
        strokeWidth="3.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <path d="M20 6 9 17l-5-5" />
      </svg>
    </span>
  );
}

function Banner({ g }: { g?: Game }) {
  return (
    <div
      className="relative h-28 overflow-hidden border-b border-separator transition-[background] duration-500"
      style={{ background: gameBackdrop(g?.id) }}
    >
      <div className="absolute bottom-4 left-5">
        <GameMark id={g?.id} name={g?.name} size="lg" />
      </div>
    </div>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-muted">{label}</dt>
      <dd className="truncate text-right text-foreground">{value || "-"}</dd>
    </div>
  );
}

function NoMachines({
  group,
  others,
  onSwitch,
}: {
  group: MeGroup;
  others: MeGroup[];
  onSwitch: (id: string) => void;
}) {
  return (
    <Callout
      title={`${group.name} has no machines yet`}
      actions={
        <>
          {others.slice(0, 2).map((g) => (
            <Button
              key={g.group_id}
              variant="secondary"
              onClick={() => onSwitch(g.group_id)}
            >
              Switch to {g.name}
            </Button>
          ))}
          <Button variant="primary" onClick={() => navigate("/machines/add")}>
            Add a machine
          </Button>
        </>
      }
    >
      Servers run on your group's PCs.{" "}
      {others.length > 0
        ? "If your PC is linked to another group, switch to it, or add a machine here."
        : "Add one to start servers here."}
    </Callout>
  );
}
