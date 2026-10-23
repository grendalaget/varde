import { useCallback, useEffect, useState } from "react";
import { api, errorMessage, type Schemas } from "../api/client";
import { Button, Card, CardHeader, ErrorNote, Field, inputClass, PageHeader } from "../components/ui";
import { useLoad } from "../lib/data";
import { useSession } from "../lib/session";

const MAPS = [
  "de_ancient",
  "de_anubis",
  "de_dust2",
  "de_inferno",
  "de_mirage",
  "de_nuke",
  "de_train",
];
const FORMATS = [
  ["shuffle", "Shuffle — new balanced teams every map"],
  ["single_elimination", "Single elimination"],
  ["double_elimination", "Double elimination"],
  ["round_robin", "Round robin"],
  ["swiss", "Swiss"],
] as const;
type Format = (typeof FORMATS)[number][0];
type Tournament = Schemas["AutoTournamentTournament"];

export default function Tournaments() {
  const { group } = useSession();
  const integration = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/integrations/autotournament", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );
  const members = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/members", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );
  const [tournaments, setTournaments] = useState<Tournament[]>([]);
  const [format, setFormat] = useState<Format>("shuffle");
  const [name, setName] = useState("");
  const [teamSize, setTeamSize] = useState(5);
  const [maps, setMaps] = useState<string[]>(["de_mirage", "de_inferno", "de_nuke"]);
  const [matchFormat, setMatchFormat] = useState<"bo1" | "bo3" | "bo5">("bo1");
  const [selected, setSelected] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [created, setCreated] = useState<Schemas["CreateGroupTournamentResponse"] | null>(null);
  const linked = integration.data?.linked ?? false;
  useEffect(() => {
    setSelected(
      (members.data?.members ?? [])
        .filter((member) => !!member.steam_id)
        .map((member) => member.user_id),
    );
  }, [members.data, group.group_id]);

  const refreshTournaments = useCallback(async () => {
    const { data, error } = await api.GET("/v1/groups/{groupId}/tournaments", {
      params: { path: { groupId: group.group_id } },
    });
    if (!error && data) setTournaments(data);
  }, [group.group_id]);

  useEffect(() => {
    if (linked) void refreshTournaments();
    else setTournaments([]);
  }, [linked, refreshTournaments]);

  if (!integration.data) {
    return (
      <>
        <PageHeader title="Tournaments" subtitle={group.name} />
        <p className="text-sm text-muted">Loading Auto Tournament link…</p>
      </>
    );
  }

  if (!linked) {
    return (
      <>
        <PageHeader title="Tournaments" subtitle={group.name} />
        <Card>
          <CardHeader title="Auto Tournament isn’t linked" />
          <div className="p-4 text-sm text-muted">
            Link an Auto Tournament instance in{" "}
            <a className="text-accent underline" href="/settings">
              Settings
            </a>{" "}
            to create and manage group tournaments.
          </div>
        </Card>
      </>
    );
  }

  const groupMembers = members.data?.members ?? [];
  return (
    <>
      <PageHeader title="Tournaments" subtitle={group.name} />
      <div className="vd-stagger space-y-6">
        <Card>
          <CardHeader title="Group tournaments" />
          {tournaments.length === 0 ? (
            <p className="p-4 text-sm text-muted">No tournaments found.</p>
          ) : (
            <ul className="divide-y divide-separator">
              {tournaments.map((tournament) => (
                <li
                  key={tournament.id}
                  className="flex flex-col gap-2 px-4 py-3 sm:flex-row sm:items-center sm:justify-between"
                >
                  <div className="min-w-0">
                    <div className="font-medium text-foreground">{tournament.name}</div>
                    <div className="text-xs text-muted">
                      {tournament.type} · {tournament.status}
                    </div>
                  </div>
                  <a
                    className="text-sm text-accent underline"
                    href={tournament.url}
                    target="_blank"
                    rel="noreferrer"
                  >
                    Open tournament
                  </a>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card>
          <CardHeader title="Start a tournament" />
          <form
            className="space-y-5 p-4"
            onSubmit={async (e) => {
              e.preventDefault();
              setBusy(true);
              setErr(null);
              setCreated(null);
              const { data, error } = await api.POST(
                "/v1/groups/{groupId}/tournaments",
                {
                  params: { path: { groupId: group.group_id } },
                  body: {
                    name: name.trim(),
                    format,
                    team_size: teamSize,
                    maps,
                    match_format: matchFormat,
                    ...(selected.length > 0 ? { user_ids: selected } : {}),
                    start: true,
                  },
                },
              );
              setBusy(false);
              if (error) setErr(errorMessage(error));
              else if (data) {
                setCreated(data);
                await refreshTournaments();
              }
            }}
          >
            <Field label="Tournament name">
              <input
                className={inputClass}
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
              />
            </Field>
            <Field label="Format">
              <select className={inputClass} value={format} onChange={(e) => setFormat(e.target.value as Format)}>
                {FORMATS.map(([value, label]) => (
                  <option key={value} value={value}>
                    {label}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Players per team">
              <select
                className={inputClass}
                value={teamSize}
                onChange={(e) => setTeamSize(Number(e.target.value))}
              >
                {[1, 2, 3, 4, 5].map((size) => (
                  <option key={size} value={size}>
                    {size}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Maps">
              <div className="flex flex-wrap gap-2">
                {MAPS.map((map) => {
                  const active = maps.includes(map);
                  return (
                    <button
                      key={map}
                      type="button"
                      aria-pressed={active}
                      className={`rounded-full border px-3 py-1.5 text-sm ${
                        active
                          ? "border-accent bg-accent/15 text-foreground"
                          : "border-field-border text-muted"
                      }`}
                      onClick={() =>
                        setMaps(active ? maps.filter((item) => item !== map) : [...maps, map])
                      }
                    >
                      {map.replace("de_", "")}
                    </button>
                  );
                })}
              </div>
              <span className="text-xs text-muted">Choose 1–7 maps.</span>
            </Field>
            {format !== "shuffle" && (
              <Field label="Match format">
                <select
                  className={inputClass}
                  value={matchFormat}
                  onChange={(e) => setMatchFormat(e.target.value as "bo1" | "bo3" | "bo5")}
                >
                  <option value="bo1">Best of 1</option>
                  <option value="bo3">Best of 3</option>
                  <option value="bo5">Best of 5</option>
                </select>
              </Field>
            )}
            <fieldset className="space-y-2">
              <legend className="mb-2 text-sm font-medium text-foreground">Players</legend>
              <div className="max-h-64 space-y-2 overflow-y-auto rounded-field border border-field-border p-3">
                {groupMembers.map((member) => {
                  const disabled = !member.steam_id;
                  return (
                    <label key={member.user_id} className="flex items-center gap-2 text-sm">
                      <input
                        type="checkbox"
                        disabled={disabled}
                        checked={selected.includes(member.user_id)}
                        onChange={(e) =>
                          setSelected(
                            e.target.checked
                              ? [...selected, member.user_id]
                              : selected.filter((id) => id !== member.user_id),
                          )
                        }
                      />
                      <span>{member.display_name}</span>
                      {disabled && <span className="text-xs text-muted">no Steam ID</span>}
                    </label>
                  );
                })}
              </div>
              <p className="text-xs text-muted">
                Leave all players unchecked to include every group member with a Steam ID.
              </p>
            </fieldset>
            <ErrorNote>{err}</ErrorNote>
            {created && (
              <div className="rounded-md border border-separator p-3 text-sm">
                <p className="font-medium text-foreground">Tournament created.</p>
                <a
                  className="text-accent underline"
                  href={created.url}
                  target="_blank"
                  rel="noreferrer"
                >
                  Open tournament
                </a>
                {created.skipped.length > 0 && (
                  <p className="mt-2 text-muted">
                    Skipped (no Steam ID): {created.skipped.join(", ")}
                  </p>
                )}
              </div>
            )}
            <div className="flex justify-end">
              <Button variant="primary" type="submit" busy={busy} disabled={maps.length < 1 || maps.length > 7}>
                Create tournament
              </Button>
            </div>
          </form>
        </Card>
      </div>
    </>
  );
}
