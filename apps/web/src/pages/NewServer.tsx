import { useEffect, useMemo, useState } from "react";
import { api, errorMessage } from "../api/client";
import {
  Button,
  Card,
  CardHeader,
  ErrorNote,
  Field,
  inputClass,
  PageHeader,
  Toggle,
} from "../components/ui";
import { navigate } from "../lib/router";
import { useSession } from "../lib/session";
import { useGames } from "../lib/games";
import { ConfigInput } from "../components/ConfigInput";

export default function NewServer() {
  const { group, nodes } = useSession();
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

  useEffect(() => {
    if (!gameId && visibleGames.length) setGameId(visibleGames[0].id);
  }, [gameId, visibleGames]);

  useEffect(() => {
    if (!game) return;
    const c: Record<string, unknown> = {};
    for (const f of game.config_fields)
      if (f.default !== undefined) c[f.name] = f.default;
    setConfig(c);
  }, [game]);

  const hosts = nodes.filter(
    (n) => n.hosting_enabled && n.admin_state === "active",
  );

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    const { data, error } = await api.POST("/v1/groups/{groupId}/servers", {
      params: { path: { groupId: group.group_id } },
      body: {
        name,
        game_id: gameId,
        config,
        preferred_node_id: preferred || undefined,
      },
    });
    if (error || !data) {
      setBusy(false);
      setErr(errorMessage(error));
      return;
    }
    if (startNow) {
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
          `The server was created, but starting it failed: ${errorMessage(startError)}`,
        );
        return;
      }
    }
    navigate(`/servers/${data.id}`);
  }

  return (
    <>
      <PageHeader title="New server" />
      <form onSubmit={submit} className="vd-stagger space-y-6">
        <Card>
          <CardHeader title="Game" />
          <div className="vd-stagger grid gap-3 p-4 sm:grid-cols-3">
            {visibleGames.map((g) => (
              <button
                type="button"
                key={g.id}
                onClick={() => setGameId(g.id)}
                className={
                  "rounded-md border px-3 py-3 text-left text-sm transition-all duration-200 ease-vd-out hover:-translate-y-0.5 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-foreground active:scale-[0.98] " +
                  (g.id === gameId
                    ? "border-foreground bg-default/30"
                    : "border-separator hover:border-field-border")
                }
              >
                <div className="font-medium">{g.name}</div>
                <div className="mt-1 text-xs text-muted">
                  Needs {Math.round((g.min_memory_mb / 1024) * 10) / 10} GB RAM
                </div>
              </button>
            ))}
          </div>
        </Card>
        <Card>
          <CardHeader title="Details" />
          <div className="space-y-4 p-4">
            <Field label="Server name">
              <input
                className={inputClass}
                required
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. Friday night server"
              />
            </Field>
            {game?.config_fields.map((f) => (
              <ConfigInput
                key={f.name}
                f={f}
                value={config[f.name]}
                onChange={(v) => setConfig((c) => ({ ...c, [f.name]: v }))}
              />
            ))}
            <Field
              label="Preferred machine"
              help="Varde prefers this machine when it's online, and uses another one otherwise."
            >
              <select
                className={inputClass}
                value={preferred}
                onChange={(e) => setPreferred(e.target.value)}
              >
                <option value="">Any machine</option>
                {hosts.map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.name}
                  </option>
                ))}
              </select>
            </Field>
            <Toggle checked={startNow} onChange={setStartNow} label="Start now">
              <span className="text-sm text-foreground">
                Start it right away
              </span>
            </Toggle>
          </div>
        </Card>
        <ErrorNote>{err}</ErrorNote>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={() => navigate("/")}>
            Cancel
          </Button>
          {createdId ? (
            <Button
              type="button"
              variant="primary"
              onClick={() => navigate(`/servers/${createdId}`)}
            >
              Open server
            </Button>
          ) : (
            <Button
              type="submit"
              variant="primary"
              busy={busy}
              disabled={!gameId}
            >
              Create server
            </Button>
          )}
        </div>
      </form>
    </>
  );
}
