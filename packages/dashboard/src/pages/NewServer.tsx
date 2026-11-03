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
import { navigate } from "../lib/router";
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
import { isWideField } from "../lib/configField";

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
  const [gameId, setGameId] = useState("");
  const [name, setName] = useState("");
  const [config, setConfig] = useState<Record<string, unknown>>({});
  const [preferred, setPreferred] = useState("");
  const [startNow, setStartNow] = useState(true);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [createdId, setCreatedId] = useState<string | null>(null);

  const visibleGames = games.filter(
    (g) => g.id !== "testgame" || import.meta.env.DEV,
  );
  const game = useMemo(
    () => games.find((g) => g.id === gameId),
    [games, gameId],
  );
  const fields = askedFields(game);
  const namesInGame = !!game && fields.length < game.config_fields.length;

  useEffect(() => {
    if (!gameId && visibleGames.length) setGameId(visibleGames[0].id);
  }, [gameId, visibleGames]);

  useEffect(() => {
    if (!game) return;
    setConfig(configDefaults(game.config_fields));
  }, [game]);

  const hosts = nodes.filter(
    (n) => n.hosting_enabled && n.admin_state === "active",
  );
  // nothing could host a start: say so up front instead of failing after create
  const canStart = !nodesLoaded || hosts.length > 0;
  const starting = startNow && canStart;
  const host = hosts.find((n) => n.id === preferred);

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
        subtitle="Pick a game and give it a name. Varde runs it on one of your group's machines."
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
          <Section n={1} title="Game">
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

          {fields.length > 0 && (
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
            n={fields.length > 0 ? 4 : 3}
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
                sub="Recommended"
              />
              {hosts.map((n) => (
                <ChoiceCard
                  key={n.id}
                  selected={preferred === n.id}
                  onSelect={() => setPreferred(n.id)}
                  title={n.name}
                  sub={
                    <>
                      <Dot tone={n.online ? "green" : "neutral"} />
                      {n.online ? "Online" : "Offline"}
                    </>
                  }
                />
              ))}
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
