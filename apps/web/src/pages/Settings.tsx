import { useEffect, useState } from "react";
import { api, errorMessage, type Schemas } from "../api/client";
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
import { PageLoader } from "../components/Logo";
import { useLoad } from "../lib/data";
import { navigate, useLocation } from "../lib/router";
import { canAdmin, useSession } from "../lib/session";
import CreateGroup from "./CreateGroup";

type GroupSettings = Schemas["GroupSettings"];

export default function Settings() {
  const { group, refreshMe, selectGroup } = useSession();
  const { me } = useSession();
  const loc = useLocation();
  const admin = canAdmin(group.role);
  const g = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );
  const [name, setName] = useState("");
  const [s, setS] = useState<GroupSettings | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [steamId, setSteamId] = useState(me.user.steam_id ?? "");
  const [profileBusy, setProfileBusy] = useState(false);
  const [profileMsg, setProfileMsg] = useState<string | null>(null);

  useEffect(() => setSteamId(me.user.steam_id ?? ""), [me.user.steam_id]);

  useEffect(() => {
    if (g.data) {
      setName(g.data.name);
      setS(g.data.settings);
    }
  }, [g.data]);

  if (loc.search.get("new") === "1") {
    return (
      <div className="max-w-sm">
        <CreateGroup
          onCreated={async (id) => {
            await refreshMe();
            selectGroup(id);
            navigate("/");
          }}
        />
      </div>
    );
  }

  if (!s) return <PageLoader />;

  const num = (k: keyof GroupSettings) => (
    <input
      type="number"
      min={1}
      className={inputClass}
      disabled={!admin}
      value={(s[k] as number | undefined) ?? ""}
      onChange={(e) =>
        setS({
          ...s,
          [k]: e.target.value === "" ? undefined : Number(e.target.value),
        })
      }
    />
  );

  return (
    <>
      <PageHeader title="Settings" subtitle={group.name} />
      <div className="vd-stagger mb-6 space-y-6">
        <Card>
          <CardHeader title="Your profile" subtitle="Player identity for Auto Tournament." />
          <form
            className="space-y-4 p-4"
            onSubmit={async (e) => {
              e.preventDefault();
              setProfileBusy(true);
              setErr(null);
              setProfileMsg(null);
              const { error } = await api.PATCH("/v1/me", {
                body: { steam_id: steamId.trim() || null },
              });
              setProfileBusy(false);
              if (error) setErr(errorMessage(error));
              else {
                setProfileMsg("Saved");
                await refreshMe();
              }
            }}
          >
            <Field
              label="Steam ID"
              help={
                <>
                  17-digit SteamID64. Find yours at{" "}
                  <a
                    href="https://steamid.io"
                    target="_blank"
                    rel="noreferrer"
                    className="text-accent underline"
                  >
                    steamid.io
                  </a>
                  .
                </>
              }
            >
              <input
                className={inputClass}
                inputMode="numeric"
                autoComplete="off"
                value={steamId}
                onChange={(e) => setSteamId(e.target.value)}
                placeholder="7656119…"
              />
            </Field>
            <ErrorNote>{err}</ErrorNote>
            <div className="flex items-center justify-end gap-3">
              {profileMsg && <span className="text-sm text-emerald-400">{profileMsg}</span>}
              <Button variant="primary" type="submit" busy={profileBusy}>
                Save profile
              </Button>
            </div>
          </form>
        </Card>
        <AutoTournamentSettings groupId={group.group_id} admin={admin} />
      </div>
      <form
        className="vd-stagger space-y-6"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setErr(null);
          setMsg(null);
          const { error } = await api.PATCH("/v1/groups/{groupId}", {
            params: { path: { groupId: group.group_id } },
            body: { name, settings: s },
          });
          setBusy(false);
          if (error) setErr(errorMessage(error));
          else {
            setMsg("Saved");
            await refreshMe();
          }
        }}
      >
        <Card>
          <CardHeader title="Group" />
          <div className="space-y-4 p-4">
            <Field label="Name">
              <input
                className={inputClass}
                disabled={!admin}
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </Field>
          </div>
        </Card>
        <Card>
          <CardHeader
            title="Saves & backups"
            subtitle="Defaults for new servers. Each server can override them."
          />
          <div className="grid gap-4 p-4 sm:grid-cols-2">
            <Field label="Save every (seconds)">
              {num("snapshot_interval_s")}
            </Field>
            <Field label="Keep saves" help="Newest safe saves kept per server">
              {num("snapshot_retention")}
            </Field>
            <Field
              label="Copies per save"
              help="How many machines should hold each save"
            >
              {num("default_replication_factor")}
            </Field>
            <Field label="Copies before a save counts as safe">
              {num("default_min_commit_replicas")}
            </Field>
            <div className="flex items-start gap-3 sm:col-span-2">
              <Toggle
                checked={!!s.require_anchor_for_commit}
                disabled={!admin}
                onChange={(v) => setS({ ...s, require_anchor_for_commit: v })}
                label="Require always-on backup"
              >
                <span>
                  <span className="block text-sm text-foreground">
                    Require the always-on backup
                  </span>
                  <span className="block text-xs text-muted">
                    A save only counts as safe once an always-on backup has it
                    (applies when the group has one).
                  </span>
                </span>
              </Toggle>
            </div>
          </div>
        </Card>
        <ErrorNote>{err}</ErrorNote>
        {admin && (
          <div className="flex items-center justify-end gap-3">
            {msg && (
              <span className="vd-fade text-sm text-emerald-400">{msg}</span>
            )}
            <Button variant="primary" type="submit" busy={busy}>
              Save settings
            </Button>
          </div>
        )}
      </form>
      {group.role === "owner" && (
        <Card className="mt-10 border-rose-900/60">
          <CardHeader
            title="Delete group"
            subtitle="Removes servers and machine registrations. Saves on machines are not wiped."
            actions={
              <Button
                variant="danger"
                onClick={async () => {
                  if (
                    prompt(
                      `Type the group name (${group.name}) to delete it`,
                    ) !== group.name
                  )
                    return;
                  const { error } = await api.DELETE("/v1/groups/{groupId}", {
                    params: { path: { groupId: group.group_id } },
                  });
                  if (error) setErr(errorMessage(error));
                  else {
                    localStorage.removeItem("varde.group");
                    window.location.href = "/";
                  }
                }}
              >
                Delete group
              </Button>
            }
          />
        </Card>
      )}
    </>
  );
}

function AutoTournamentSettings({
  groupId,
  admin,
}: {
  groupId: string;
  admin: boolean;
}) {
  const link = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/integrations/autotournament", {
        params: { path: { groupId } },
      }),
    [groupId],
  );
  const [baseUrl, setBaseUrl] = useState("");
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const linked = link.data?.linked ?? false;

  useEffect(() => {
    setBaseUrl(link.data?.base_url ?? "");
  }, [groupId, link.data?.base_url]);

  return (
    <Card>
      <CardHeader
        title="Auto Tournament"
        subtitle={linked ? `Linked to ${link.data?.base_url}` : "Not linked"}
      />
      <div className="space-y-4 p-4">
        {admin && (
          <form
            className="space-y-4"
            onSubmit={async (e) => {
              e.preventDefault();
              setBusy(true);
              setErr(null);
              setMsg(null);
              const { error } = await api.PUT(
                "/v1/groups/{groupId}/integrations/autotournament",
                {
                  params: { path: { groupId } },
                  body: { base_url: baseUrl.trim(), api_token: token },
                },
              );
              setBusy(false);
              if (error) setErr(errorMessage(error));
              else {
                setToken("");
                setMsg("Linked");
                void link.refresh();
              }
            }}
          >
            <Field label="Base URL">
              <input
                className={inputClass}
                type="url"
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
                placeholder="https://tournaments.example.com"
                required
              />
            </Field>
            <Field label="API token" help="The token is stored encrypted and never shown again.">
              <input
                className={inputClass}
                type="password"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                autoComplete="new-password"
                required
              />
            </Field>
            <div className="flex flex-wrap items-center justify-end gap-3">
              {msg && <span className="text-sm text-emerald-400">{msg}</span>}
              {linked && (
                <Button
                  variant="danger"
                  busy={busy}
                  onClick={async () => {
                    setBusy(true);
                    setErr(null);
                    const { error } = await api.DELETE(
                      "/v1/groups/{groupId}/integrations/autotournament",
                      { params: { path: { groupId } } },
                    );
                    setBusy(false);
                    if (error) setErr(errorMessage(error));
                    else void link.refresh();
                  }}
                >
                  Unlink
                </Button>
              )}
              <Button variant="primary" type="submit" busy={busy}>
                {linked ? "Update link" : "Link"}
              </Button>
            </div>
          </form>
        )}
        {!admin && <p className="text-sm text-muted">{linked ? "Linked" : "Not linked"}</p>}
        <ErrorNote>{err}</ErrorNote>
        <p className="text-xs text-muted">Auto Tournament is licensed for noncommercial use.</p>
      </div>
    </Card>
  );
}
