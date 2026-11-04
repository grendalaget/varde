import { useEffect, useState } from "react";
import { api, errorMessage, type Schemas } from "../api/client";
import { ConfigInput } from "../components/ConfigInput";
import { initialFieldValue, updateFieldValue } from "../lib/configField";
import { Button, Card, CardHeader, ErrorNote } from "../components/ui";
import { useLoad } from "../lib/data";
import { canAdmin, useSession } from "../lib/session";
import { Link } from "../lib/router";
import {
  isManagedMode,
  managedPhaseLabel,
  managedPluginError,
  managedPublicURL,
  pluginFields,
  pluginSettingsPayload,
} from "../lib/managedPlugin";

type Plugin = Schemas["Plugin"];
type GroupPlugin = Schemas["GroupPlugin"];

export default function PluginSettingsPanel({
  className = "",
  pluginId,
  onClose,
}: {
  className?: string;
  pluginId?: string;
  onClose?: () => void;
}) {
  const { group } = useSession();
  const catalog = useLoad(() => api.GET("/v1/plugins"), []);
  const games = useLoad(() => api.GET("/v1/games"), []);
  const enabled = useLoad(
    () => api.GET("/v1/groups/{groupId}/plugins", { params: { path: { groupId: group.group_id } } }),
    [group.group_id],
  );
  const members = useLoad(
    () => api.GET("/v1/groups/{groupId}/members", { params: { path: { groupId: group.group_id } } }),
    [group.group_id],
  );
  const plugins = catalog.data?.plugins ?? [];
  if (!plugins.length) return null;
  if (pluginId) {
    const plugin = plugins.find((item) => item.id === pluginId);
    if (!plugin) return null;
    return (
      <PluginSettings
        plugin={plugin}
        groupId={group.group_id}
        canEdit={canAdmin(group.role)}
        record={enabled.data?.plugins.find((item) => item.id === plugin.id)}
        game={games.data?.games.find(
          (item) => item.id === plugin.managed_server?.game,
        )}
        members={members.data?.members ?? []}
        refresh={enabled.refresh}
        onClose={onClose}
      />
    );
  }
  return (
    <Card className={className}>
      <CardHeader title="Plugins" subtitle="Optional integrations provided by separately hosted services." />
      <div className="space-y-3 p-4">
        {plugins.map((plugin) => (
          <PluginSettings
            key={plugin.id}
            plugin={plugin}
            groupId={group.group_id}
            canEdit={canAdmin(group.role)}
            record={enabled.data?.plugins.find((item) => item.id === plugin.id)}
            game={games.data?.games.find(
              (item) => item.id === plugin.managed_server?.game,
            )}
            members={members.data?.members ?? []}
            refresh={enabled.refresh}
          />
        ))}
      </div>
    </Card>
  );
}

function PluginSettings({
  plugin,
  groupId,
  canEdit,
  record,
  game,
  members,
  refresh,
  onClose,
}: {
  plugin: Plugin;
  groupId: string;
  canEdit: boolean;
  record?: GroupPlugin;
  game?: Schemas["Game"];
  members: Schemas["Member"][];
  refresh: () => Promise<void>;
  onClose?: () => void;
}) {
  const [settings, setSettings] = useState<Record<string, unknown>>({});
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [editedSecrets, setEditedSecrets] = useState<Set<string>>(new Set());
  const managed = isManagedMode(plugin, settings);
  const params = { path: { groupId, pluginId: plugin.id } };
  const status = useLoad<Schemas["ManagedServerStatus"]>(
    () =>
      managed && canEdit
        ? api.GET("/v1/groups/{groupId}/plugins/{pluginId}/managed-server", {
            params,
          })
        : Promise.resolve({}),
    [managed, canEdit, groupId, plugin.id],
  );
  const phase = status.data?.phase;
  const refreshStatus = status.refresh;
  const running =
    phase === "preparing" || phase === "starting" || phase === "ready";
  const publicURL = managedPublicURL(status.data?.public_url);
  const failure = managedPluginError(error, plugin, game);
  useEffect(() => {
    if (!managed || !canEdit || (phase !== "preparing" && phase !== "starting"))
      return;
    const timer = setInterval(() => void refreshStatus(), 3000);
    return () => clearInterval(timer);
  }, [managed, canEdit, phase, refreshStatus]);
  useEffect(() => {
    const initial: Record<string, unknown> = {};
    for (const field of plugin.settings) {
      if (field.hidden) continue;
      const stored = field.type === "secret" || field.secret
        ? undefined
        : record?.settings[field.name];
      const value = initialFieldValue(field, stored);
      if (value !== undefined) initial[field.name] = value;
    }
    setSettings(initial);
    setEditedSecrets(new Set());
  }, [plugin, record, groupId]);

  async function save(start = false) {
    setBusy(true);
    setError(null);
    setMessage(null);
    try {
      const { error: saveError } = await api.PUT(
        "/v1/groups/{groupId}/plugins/{pluginId}",
        {
          params,
          body: {
            settings: pluginSettingsPayload(plugin, settings, editedSecrets),
          },
        },
      );
      if (saveError) throw saveError;
      await refresh();
      window.dispatchEvent(new Event("varde:plugins-changed"));
      if (start || (managed && running)) {
        const { data, error: startError } = await api.POST(
          "/v1/groups/{groupId}/plugins/{pluginId}/managed-server/start",
          { params },
        );
        if (startError) throw startError;
        status.setData(data);
      }
      await status.refresh();
      setMessage("Saved");
    } catch (err) {
      setError(err);
      await status.refresh();
    } finally {
      setBusy(false);
    }
  }

  async function stop() {
    if (!confirm(`Stop the server for ${plugin.name}?`)) return;
    setBusy(true);
    setError(null);
    setMessage(null);
    try {
      const { data, error: stopError } = await api.POST(
        "/v1/groups/{groupId}/plugins/{pluginId}/managed-server/stop",
        { params },
      );
      if (stopError) throw stopError;
      status.setData(data);
      await status.refresh();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  async function disable() {
    if (!confirm(`Disable ${plugin.name} for this group?`)) return;
    setBusy(true);
    setError(null);
    setMessage(null);
    try {
      const { error: disableError } = await api.DELETE(
        "/v1/groups/{groupId}/plugins/{pluginId}",
        { params },
      );
      if (disableError) throw disableError;
      setMessage("Disabled");
      await refresh();
      await status.refresh();
      window.dispatchEvent(new Event("varde:plugins-changed"));
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  const secretNames = record?.secrets_set ?? [];
  return (
    <div className="rounded-xl border border-separator">
      <CardHeader
        title={plugin.name}
        subtitle={`${plugin.version}, ${record?.enabled ? "Enabled" : "Not enabled"}`}
        actions={
          onClose && (
            <Button variant="ghost" onClick={onClose}>
              Close
            </Button>
          )
        }
      />
      <div className="space-y-4 p-4">
        {pluginFields(plugin, settings).map((field) => (
          <div key={field.name} className="space-y-1">
            <ConfigInput
              f={{
                ...field,
                help: [
                  field.help,
                  (field.type === "secret" || field.secret) &&
                  secretNames.includes(field.name)
                    ? "Set. Leave blank to keep the current value."
                    : undefined,
                ]
                  .filter(Boolean)
                  .join(" "),
              }}
              value={settings[field.name]}
              onChange={(value) => {
                setSettings((current) =>
                  updateFieldValue(current, field.name, value),
                );
                if (field.name === plugin.managed_server?.mode_setting)
                  setError(null);
                if (field.type === "secret" || field.secret)
                  setEditedSecrets((current) => {
                    const next = new Set(current);
                    if (value) next.add(field.name);
                    else next.delete(field.name);
                    return next;
                  });
              }}
              members={members}
              disabled={!canEdit || busy}
            />
            {canEdit &&
              secretNames.includes(field.name) &&
              Object.values(plugin.managed_server?.config ?? {}).includes(
                field.name,
              ) &&
              (editedSecrets.has(field.name) && settings[field.name] === "" ? (
                <p className="text-xs text-muted">
                  Will be removed when saved.
                </p>
              ) : (
                <Button
                  variant="ghost"
                  disabled={busy}
                  onClick={() => {
                    setSettings((current) => ({
                      ...current,
                      [field.name]: "",
                    }));
                    setEditedSecrets((current) =>
                      new Set(current).add(field.name),
                    );
                  }}
                >
                  Clear saved value
                </Button>
              ))}
          </div>
        ))}
        {managed && canEdit && (
          <div
            className="space-y-3 rounded-lg border border-separator p-4"
            aria-live="polite"
          >
            <div className="flex items-center justify-between gap-3">
              <div className="space-y-1">
                <h3 className="text-sm font-medium">Managed server</h3>
                {!status.data && status.loading && (
                  <p className="text-sm text-muted">Checking status</p>
                )}
                {phase && phase !== "stopped" && (
                  <p className="text-sm">{managedPhaseLabel(phase)}</p>
                )}
                {status.data?.node_name && (
                  <p className="text-sm text-muted">
                    Running on {status.data.node_name}
                  </p>
                )}
              </div>
              {running && (
                <Button variant="ghost" disabled={busy} onClick={stop}>
                  Stop
                </Button>
              )}
            </div>
            {phase === "failed" && <ErrorNote>{status.data?.reason}</ErrorNote>}
            {status.error != null && (
              <ErrorNote>
                {errorMessage(status.error)}{" "}
                <Button
                  variant="ghost"
                  disabled={busy}
                  onClick={() => void status.refresh()}
                >
                  Retry
                </Button>
              </ErrorNote>
            )}
            {phase === "ready" && publicURL && (
              <div className="flex flex-wrap items-center gap-3 text-sm">
                <a
                  className="break-all text-foreground underline underline-offset-2"
                  href={publicURL}
                  target="_blank"
                  rel="noreferrer"
                >
                  {status.data?.public_url}
                </a>
                <CopyButton text={publicURL} />
              </div>
            )}
            {status.data?.setup_code && (
              <div className="space-y-1">
                <p className="text-sm text-muted">Admin setup code</p>
                <div className="flex flex-wrap items-center gap-3">
                  <code className="rounded bg-field px-2 py-1 text-sm">
                    {status.data.setup_code}
                  </code>
                  <CopyButton text={status.data.setup_code} />
                </div>
              </div>
            )}
          </div>
        )}
        <ErrorNote>
          {failure && (
            <>
              {failure.message}
              {failure.needsPC && (
                <>
                  {" "}
                  <Link
                    className="underline underline-offset-2"
                    to="/machines/add"
                  >
                    Add one under Machines
                  </Link>
                  .
                </>
              )}
            </>
          )}
        </ErrorNote>
        <div className="flex items-center justify-end gap-3">
          {message && <span className="text-sm text-emerald-400">{message}</span>}
          {canEdit && (
            <>
              {record?.enabled && <Button variant="ghost" disabled={busy} onClick={disable}>Disable</Button>}
              <Button
                variant="primary"
                disabled={
                  busy || (managed && (!status.data || status.error != null))
                }
                busy={busy}
                onClick={() =>
                  void save(
                    managed &&
                      (!record?.enabled ||
                        phase === "stopped" ||
                        phase === "failed"),
                  )
                }
              >
                {managed && !record?.enabled
                  ? "Enable and start"
                  : managed && (phase === "stopped" || phase === "failed")
                    ? "Start"
                    : record?.enabled
                      ? "Save plugin settings"
                      : "Enable plugin"}
              </Button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState(false);
  useEffect(() => {
    setCopied(false);
    setError(false);
  }, [text]);
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 1400);
    return () => clearTimeout(timer);
  }, [copied]);
  return (
    <>
      <Button
        variant="ghost"
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(text);
            setCopied(true);
            setError(false);
          } catch {
            setError(true);
          }
        }}
      >
        {copied ? "Copied" : "Copy"}
      </Button>
      {error && (
        <span className="text-xs text-muted">Select the text to copy it.</span>
      )}
    </>
  );
}
