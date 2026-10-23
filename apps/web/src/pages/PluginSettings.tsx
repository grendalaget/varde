import { useEffect, useState } from "react";
import { api, errorMessage, type Schemas } from "../api/client";
import { ConfigInput } from "../components/ConfigInput";
import { Button, Card, CardHeader, ErrorNote } from "../components/ui";
import { useLoad } from "../lib/data";
import { canAdmin, useSession } from "../lib/session";

type Plugin = Schemas["Plugin"];
type GroupPlugin = Schemas["GroupPlugin"];

export default function PluginSettingsPanel({ className = "" }: { className?: string }) {
  const { group } = useSession();
  const catalog = useLoad(() => api.GET("/v1/plugins"), []);
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
  members,
  refresh,
}: {
  plugin: Plugin;
  groupId: string;
  canEdit: boolean;
  record?: GroupPlugin;
  members: Schemas["Member"][];
  refresh: () => Promise<void>;
}) {
  const [settings, setSettings] = useState<Record<string, unknown>>({});
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    const initial: Record<string, unknown> = {};
    for (const field of plugin.settings) {
      if (field.hidden) continue;
      if (field.type === "secret" || field.secret) initial[field.name] = "";
      else initial[field.name] = record?.settings[field.name] ?? field.default ??
        (field.type === "multiselect" || field.type === "member_multiselect" ? [] : "");
    }
    setSettings(initial);
  }, [plugin, record]);

  async function save() {
    setBusy(true);
    setError(null);
    setMessage(null);
    const { error: saveError } = await api.PUT("/v1/groups/{groupId}/plugins/{pluginId}", {
      params: { path: { groupId, pluginId: plugin.id } },
      body: { settings },
    });
    setBusy(false);
    if (saveError) setError(errorMessage(saveError));
    else {
      setMessage("Saved");
      await refresh();
      window.dispatchEvent(new Event("varde:plugins-changed"));
    }
  }

  async function disable() {
    if (!confirm(`Disable ${plugin.name} for this group?`)) return;
    setBusy(true);
    setError(null);
    const { error: disableError } = await api.DELETE("/v1/groups/{groupId}/plugins/{pluginId}", {
      params: { path: { groupId, pluginId: plugin.id } },
    });
    setBusy(false);
    if (disableError) setError(errorMessage(disableError));
    else {
      setMessage("Disabled");
      await refresh();
      window.dispatchEvent(new Event("varde:plugins-changed"));
    }
  }

  const secretNames = record?.secrets_set ?? [];
  return (
    <div className="rounded-xl border border-separator">
      <CardHeader
        title={plugin.name}
        subtitle={`${plugin.version} · ${record?.enabled ? "Enabled" : "Not enabled"}`}
      />
      <div className="space-y-4 p-4">
        {plugin.settings.filter((field) => !field.hidden).map((field) => (
          <ConfigInput
            key={field.name}
            f={{
              ...field,
              help: field.type === "secret" || field.secret
                ? secretNames.includes(field.name)
                  ? "Set — leave blank to keep."
                  : "Secret value."
                : field.help,
            }}
            value={settings[field.name]}
            onChange={(value) => setSettings((current) => ({ ...current, [field.name]: value }))}
            members={members}
            disabled={!canEdit}
          />
        ))}
        <ErrorNote>{error}</ErrorNote>
        <div className="flex items-center justify-end gap-3">
          {message && <span className="text-sm text-emerald-400">{message}</span>}
          {canEdit && (
            <>
              {record?.enabled && <Button variant="ghost" disabled={busy} onClick={disable}>Disable</Button>}
              <Button variant="primary" disabled={busy} busy={busy} onClick={save}>
                {record?.enabled ? "Save plugin settings" : "Enable plugin"}
              </Button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
