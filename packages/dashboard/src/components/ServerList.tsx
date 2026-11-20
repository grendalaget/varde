import type { Server } from "../api/client";
import { timeAgo } from "../lib/format";
import { Link } from "../lib/router";
import { HostMarker, StateCairn } from "./Logo";
import SaveSafety from "./SaveSafety";
import { CopyText } from "./ui";

type Entry = { server: Server; gameName: string; crossplay: boolean };
const columns = [
  "Server",
  "Status",
  "Host",
  "Address",
  "Latest safe save",
  "Backups",
];

function Status({ server }: { server: Server }) {
  return (
    <span
      className={`server-list-status ${server.observed_state === "stopped" ? "server-list-stopped" : ""}`}
    >
      <StateCairn state={server.observed_state} />
      {server.observed_state === "failed"
        ? "Needs attention"
        : server.observed_state.replace(/^./, (c) => c.toUpperCase())}
    </span>
  );
}

function Host({ server }: { server: Server }) {
  const host = server.summary.hosting_on;
  return host ? (
    <HostMarker nodeId={host.node_id} name={host.name} />
  ) : (
    <span className="text-muted">Not running</span>
  );
}

function Address({ server: s, crossplay }: Entry) {
  if (s.public_url)
    return (
      <CopyText text={s.public_url} href={s.public_url} mono={false} plain />
    );
  if (crossplay)
    return s.summary.join_code ? (
      <CopyText text={s.summary.join_code} plain />
    ) : (
      <span className="text-muted">
        {s.observed_state === "running" ? "Waiting for code…" : "—"}
      </span>
    );
  return s.summary.address ? (
    <CopyText text={s.summary.address} plain />
  ) : (
    <span className="text-muted">—</span>
  );
}

function LatestSave({ server }: { server: Server }) {
  const save = server.summary.latest_safe_save;
  return save ? (
    <time dateTime={new Date(save.created_at).toISOString()}>
      {timeAgo(save.created_at)}
    </time>
  ) : (
    <span className="text-muted">None yet</span>
  );
}

function ConnectionHint({ server, crossplay }: Entry) {
  if (server.public_url) return null;
  if (crossplay)
    return (
      <p className="server-connection-hint">
        Join in Valheim via Join game → Join by code. Check here after a restart
        or move for the current code.
      </p>
    );
  if (server.game_id === "cs2")
    return (
      <p className="server-connection-hint">
        In CS2, open the console and run{" "}
        <code>connect {server.summary.address || "<address>"}</code>.
      </p>
    );
  return null;
}

export function ServerList({ entries }: { entries: Entry[] }) {
  return (
    <>
      <div className="vd-surface server-table-surface">
        <table className="server-table" aria-label="Servers">
          <colgroup>
            {[22, 11, 13, 22, 13, 19].map((w, i) => (
              <col key={i} style={{ width: `${w}%` }} />
            ))}
          </colgroup>
          <thead>
            <tr>
              {columns.map((c) => (
                <th key={c} scope="col" className="vd-label">
                  {c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => {
              const s = entry.server;
              return (
                <tr key={s.id}>
                  <th scope="row">
                    <Link className="server-list-name" to={`/servers/${s.id}`}>
                      {s.name}
                    </Link>
                    <p className="server-list-game">{entry.gameName}</p>
                  </th>
                  <td>
                    <Status server={s} />
                  </td>
                  <td>
                    <Host server={s} />
                  </td>
                  <td>
                    <Address {...entry} />
                    <ConnectionHint {...entry} />
                  </td>
                  <td>
                    <LatestSave server={s} />
                  </td>
                  <td className="server-backups">
                    <SaveSafety summary={s.summary} compact plain />
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <div className="server-cards">
        {entries.map((entry) => {
          const s = entry.server;
          return (
            <article className="vd-surface server-list-card" key={s.id}>
              <header>
                <div>
                  <h2>
                    <Link className="server-list-name" to={`/servers/${s.id}`}>
                      {s.name}
                    </Link>
                  </h2>
                  <p className="server-list-game">{entry.gameName}</p>
                </div>
                <Status server={s} />
              </header>
              <dl>
                <div>
                  <dt>Host</dt>
                  <dd>
                    <Host server={s} />
                  </dd>
                </div>
                <div>
                  <dt>
                    {!s.public_url && entry.crossplay ? "Join code" : "Address"}
                  </dt>
                  <dd>
                    <Address {...entry} />
                  </dd>
                </div>
                <div>
                  <dt>Latest safe save</dt>
                  <dd>
                    <LatestSave server={s} />
                  </dd>
                </div>
                <div className="server-backups">
                  <dt>Backups</dt>
                  <dd>
                    <SaveSafety summary={s.summary} compact plain />
                  </dd>
                </div>
              </dl>
              <ConnectionHint {...entry} />
            </article>
          );
        })}
      </div>
    </>
  );
}
