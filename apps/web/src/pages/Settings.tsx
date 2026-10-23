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
import PluginSettingsPanel from "./PluginSettings";

type GroupSettings = Schemas["GroupSettings"];

export default function Settings() {
  const { group, me, refreshMe, selectGroup } = useSession();
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
  const [profileBusy, setProfileBusy] = useState(false);
  const [profileMessage, setProfileMessage] = useState<string | null>(null);
  const [profileError, setProfileError] = useState<string | null>(null);

  useEffect(() => {
    const steam = loc.search.get("steam");
    const error = loc.search.get("steam_error");
    if (steam === "linked") {
      setProfileMessage("Steam account linked.");
      void refreshMe();
    }
    if (error) {
      const messages: Record<string, string> = {
        session: "Link expired, try again",
        expired: "Link expired, try again",
        cancelled: "Cancelled on Steam",
        invalid: "Steam couldn't verify the sign-in",
        unavailable: "Steam couldn't verify the sign-in",
        taken: "That Steam account is linked to another Varde user",
      };
      setProfileError(messages[error] ?? "Steam couldn't verify the sign-in");
    }
    if (steam || error) {
      const url = new URL(window.location.href);
      url.searchParams.delete("steam");
      url.searchParams.delete("steam_error");
      window.history.replaceState(window.history.state, "", url.toString());
    }
  }, [loc.search, refreshMe]);

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
      <Card className="mt-6">
        <CardHeader title="Your profile" />
        <div className="space-y-4 p-4">
          {me.user.steam_id ? (
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <div className="flex min-w-0 items-center gap-3">
                {me.user.steam_avatar_url && (
                  <img
                    src={me.user.steam_avatar_url}
                    alt=""
                    className="h-6 w-6 shrink-0 rounded-full object-cover"
                  />
                )}
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium text-foreground">
                    {me.user.steam_persona_name || me.user.steam_id}
                  </p>
                  <p className="break-all text-xs text-muted">{me.user.steam_id}</p>
                </div>
              </div>
              <Button
                variant="secondary"
                className="w-full sm:w-auto"
                type="button"
                busy={profileBusy}
                onClick={async () => {
                  setProfileBusy(true);
                  setProfileError(null);
                  setProfileMessage(null);
                  const { error } = await api.DELETE("/v1/me/steam");
                  setProfileBusy(false);
                  if (error) setProfileError(errorMessage(error));
                  else {
                    setProfileMessage("Steam account unlinked.");
                    await refreshMe();
                  }
                }}
              >
                Unlink
              </Button>
            </div>
          ) : (
            <Button
              variant="primary"
              className="w-full sm:w-auto"
              type="button"
              busy={profileBusy}
              onClick={async () => {
                setProfileBusy(true);
                setProfileError(null);
                setProfileMessage(null);
                const { data, error } = await api.POST("/v1/me/steam/link");
                setProfileBusy(false);
                if (error) setProfileError(errorMessage(error));
                else if (data) window.location.href = data.redirect_url;
              }}
            >
              Sign in through Steam
            </Button>
          )}
          <ErrorNote>{profileError}</ErrorNote>
          {profileMessage && <p className="text-sm text-emerald-400">{profileMessage}</p>}
        </div>
      </Card>
      <PluginSettingsPanel className="mt-6" />
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
