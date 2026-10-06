import { useEffect, useMemo, useState } from "react";
import { api, errorMessage, type GameConfigField } from "../api/client";
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
      await api.POST("/v1/servers/{serverId}/start", {
        params: { path: { serverId: data.id } },
        body: {},
      });
    }
    navigate(`/servers/${data.id}`);
  }

  return (
    <>
      <PageHeader title="New server" />
      <form onSubmit={submit} className="space-y-6">
        <Card>
          <CardHeader title="Game" />
          <div className="grid gap-3 p-4 sm:grid-cols-3">
            {visibleGames.map((g) => (
              <button
                type="button"
                key={g.id}
                onClick={() => setGameId(g.id)}
                className={
                  "rounded-md border px-3 py-3 text-left text-sm " +
                  (g.id === gameId
                    ? "border-take bg-take/5"
                    : "border-skifer-800 hover:border-skifer-600")
                }
              >
                <div className="font-medium">{g.name}</div>
                <div className="mt-1 text-xs text-skifer-400">
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
            <div className="flex items-center gap-3">
              <Toggle
                checked={startNow}
                onChange={setStartNow}
                label="Start now"
              />
              <span className="text-sm text-skifer-300">
                Start it right away
              </span>
            </div>
          </div>
        </Card>
        <ErrorNote>{err}</ErrorNote>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={() => navigate("/")}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant="primary"
            busy={busy}
            disabled={!gameId}
          >
            Create server
          </Button>
        </div>
      </form>
    </>
  );
}

export function ConfigInput({
  f,
  value,
  onChange,
}: {
  f: GameConfigField;
  value: unknown;
  onChange: (v: unknown) => void;
}) {
  const label = f.name
    .replace(/_/g, " ")
    .replace(/^\w/, (c) => c.toUpperCase());
  if (f.type === "bool") {
    return (
      <div className="flex items-start gap-3">
        <Toggle checked={!!value} onChange={onChange} label={label} />
        <div>
          <div className="text-sm text-skifer-200">{label}</div>
          {f.help && <div className="text-xs text-skifer-400">{f.help}</div>}
        </div>
      </div>
    );
  }
  return (
    <Field label={label} help={f.help}>
      {f.type === "select" ? (
        <select
          className={inputClass}
          value={String(value ?? "")}
          onChange={(e) => onChange(e.target.value)}
        >
          {(f.options ?? []).map((o) => (
            <option key={String(o)} value={String(o)}>
              {String(o)}
            </option>
          ))}
        </select>
      ) : (
        <input
          className={inputClass}
          type={
            f.type === "int"
              ? "number"
              : f.type === "secret" || f.secret
                ? "password"
                : "text"
          }
          required={f.required}
          value={value == null ? "" : String(value)}
          onChange={(e) =>
            onChange(
              f.type === "int"
                ? e.target.value === ""
                  ? undefined
                  : Number(e.target.value)
                : e.target.value,
            )
          }
        />
      )}
    </Field>
  );
}
