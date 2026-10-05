import { api, type Server } from "../api/client";
import {
  Badge,
  Button,
  Card,
  CopyText,
  Empty,
  PageHeader,
  ServerStateBadge,
} from "../components/ui";
import { useLoad } from "../lib/data";
import { useOnEvent } from "../lib/events";
import { timeAgo } from "../lib/format";
import { Link, navigate } from "../lib/router";
import { useSession } from "../lib/session";
import SaveSafety from "../components/SaveSafety";
import { useGames } from "../lib/games";

export default function Servers() {
  const { group } = useSession();
  const games = useGames();
  const { data, refresh, loading } = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/servers", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );
  useOnEvent(
    (e) =>
      !!e.server_id ||
      e.type.startsWith("snapshot.") ||
      e.type.startsWith("node."),
    refresh,
  );
  const servers = data?.servers ?? [];

  return (
    <>
      <PageHeader
        title="Servers"
        subtitle="Each server runs on one machine at a time and is backed up on the others."
        actions={
          <Button variant="primary" onClick={() => navigate("/servers/new")}>
            New server
          </Button>
        }
      />
      {!loading && servers.length === 0 && (
        <Card>
          <Empty>
            No servers yet.{" "}
            <Link
              to="/servers/new"
              className="text-emerald-400 hover:underline"
            >
              Create your first one
            </Link>
            .
          </Empty>
        </Card>
      )}
      <div className="grid gap-4 md:grid-cols-2">
        {servers.map((s) => (
          <ServerCard key={s.id} s={s} gameName={games.name(s.game_id)} />
        ))}
      </div>
    </>
  );
}

function ServerCard({ s, gameName }: { s: Server; gameName: string }) {
  const sum = s.summary;
  const crossplay = s.config.crossplay === true;
  return (
    <Link
      to={`/servers/${s.id}`}
      className="block rounded-lg border border-slate-800 bg-slate-900/60 p-4 transition-colors hover:border-slate-600"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="truncate font-semibold text-slate-50">{s.name}</h3>
          <p className="text-xs text-slate-400">{gameName}</p>
        </div>
        <ServerStateBadge state={s.observed_state} />
      </div>
      <dl className="mt-4 space-y-2 text-sm">
        <Row label="Hosting on">
          {sum.hosting_on ? (
            <span className="text-slate-100">{sum.hosting_on.name}</span>
          ) : (
            <span className="text-slate-500">Not running</span>
          )}
        </Row>
        <Row label={crossplay ? "Join code" : "Address"}>
          {crossplay ? (
            sum.join_code ? (
              <span onClick={(e) => e.preventDefault()}>
                <CopyText text={sum.join_code} />
              </span>
            ) : (
              <span className="text-slate-500">
                {s.observed_state === "running" ? "Waiting for code…" : "—"}
              </span>
            )
          ) : (
            sum.address ? (
              <span onClick={(e) => e.preventDefault()}>
                <CopyText text={sum.address} />
              </span>
            ) : (
              <span className="text-slate-500">—</span>
            )
          )}
        </Row>
        {crossplay && (
          <p className="text-xs text-slate-400">
            Crossplay server: players join from Valheim&apos;s Join game → Join
            by code. The code can change when the server restarts or moves to
            another machine, so check here for the current one.
          </p>
        )}
        <Row label="Latest safe save">
          {sum.latest_safe_save ? (
            <span className="text-slate-100">
              {timeAgo(sum.latest_safe_save.created_at)}
            </span>
          ) : (
            <span className="text-slate-500">None yet</span>
          )}
        </Row>
      </dl>
      <div className="mt-3">
        <SaveSafety summary={sum} compact />
      </div>
      {s.desired_state === "running" && s.observed_state === "failed" && (
        <div className="mt-2">
          <Badge tone="red">Needs attention</Badge>
        </div>
      )}
    </Link>
  );
}

function Row({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between gap-3">
      <dt className="text-slate-400">{label}</dt>
      <dd className="min-w-0 truncate text-right">{children}</dd>
    </div>
  );
}
