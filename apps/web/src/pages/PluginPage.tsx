import { useEffect, useState } from "react";
import { api, errorMessage, type Schemas } from "../api/client";
import { ConfigInput } from "../components/ConfigInput";
import { Badge, Button, Card, CardHeader, Empty, ErrorNote, PageHeader } from "../components/ui";
import { PageLoader } from "../components/Logo";
import { useLoad } from "../lib/data";
import { useSession } from "../lib/session";

export default function PluginPage({ pluginId, pageId }: { pluginId: string; pageId: string }) {
  const { group } = useSession();
  const catalog = useLoad(() => api.GET("/v1/plugins"), []);
  const groupPlugins = useLoad(
    () => api.GET("/v1/groups/{groupId}/plugins", { params: { path: { groupId: group.group_id } } }),
    [group.group_id],
  );
  const members = useLoad(
    () => api.GET("/v1/groups/{groupId}/members", { params: { path: { groupId: group.group_id } } }),
    [group.group_id],
  );
  const plugin = catalog.data?.plugins.find((item) => item.id === pluginId);
  const page = plugin?.pages.find((item) => item.id === pageId);
  const enabled = groupPlugins.data?.plugins.find((item) => item.id === pluginId)?.enabled;
  const list = useLoad(
    () => page?.list_endpoint
      ? api.POST("/v1/groups/{groupId}/plugins/{pluginId}/pages/{pageId}/list", {
          params: { path: { groupId: group.group_id, pluginId, pageId } },
        })
      : Promise.resolve({ data: undefined, error: undefined }),
    [group.group_id, pluginId, pageId, page?.list_endpoint],
  );
  if (!plugin || !page || enabled === undefined) return <PageLoader />;
  if (!enabled) return <ErrorNote>This plugin is not enabled for this group.</ErrorNote>;
  return (
    <>
      <PageHeader title={page.title} subtitle={plugin.name} />
      {page.list_endpoint && (
        <Card className="mb-5">
          <CardHeader title="Items" actions={<Button onClick={() => void list.refresh()}>Refresh</Button>} />
          <ErrorNote>{list.error ? errorMessage(list.error) : null}</ErrorNote>
          <div className="divide-y divide-separator">
            {(list.data?.items ?? []).map((item, index) => (
              <div key={`${item.title}-${index}`} className="flex items-center justify-between gap-3 px-4 py-3">
                <div className="min-w-0">
                  <div className="font-medium text-foreground">{item.title}</div>
                  {item.subtitle && <div className="text-sm text-muted">{item.subtitle}</div>}
                </div>
                <div className="flex items-center gap-2">
                  {item.status && <Badge>{item.status}</Badge>}
                  {item.href && <a className="shrink-0 text-sm underline" href={item.href} target="_blank" rel="noreferrer">Open</a>}
                </div>
              </div>
            ))}
            {!list.data?.items.length && !list.error && <Empty>No items yet.</Empty>}
          </div>
        </Card>
      )}
      <div className="space-y-5">
        {page.actions.map((action) => (
          <PluginActionForm
            key={action.id}
            groupId={group.group_id}
            pluginId={plugin.id}
            pageId={page.id}
            action={action}
            members={members.data?.members ?? []}
          />
        ))}
      </div>
    </>
  );
}

function PluginActionForm({
  groupId, pluginId, pageId, action, members,
}: {
  groupId: string;
  pluginId: string;
  pageId: string;
  action: Schemas["PluginAction"];
  members: Schemas["Member"][];
}) {
  const [values, setValues] = useState<Record<string, unknown>>({});
  const [result, setResult] = useState<Schemas["PluginActionResponse"] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    const initial: Record<string, unknown> = {};
    for (const field of action.fields) {
      initial[field.name] = field.default ??
        (field.type === "multiselect" || field.type === "member_multiselect" ? [] : "");
    }
    setValues(initial);
  }, [action]);
  return (
    <Card>
      <CardHeader title={action.title} />
      <form
        className="space-y-4 p-4"
        onSubmit={async (event) => {
          event.preventDefault();
          setBusy(true);
          setError(null);
          setResult(null);
          const { data, error: actionError } = await api.POST(
            "/v1/groups/{groupId}/plugins/{pluginId}/pages/{pageId}/actions/{actionId}",
            { params: { path: { groupId, pluginId, pageId, actionId: action.id } }, body: { values } },
          );
          setBusy(false);
          if (actionError) setError(errorMessage(actionError));
          else if (data) setResult(data);
        }}
      >
        {action.fields.map((field) => (
          <ConfigInput
            key={field.name}
            f={field}
            value={values[field.name]}
            onChange={(value) => setValues((current) => ({ ...current, [field.name]: value }))}
            members={members}
          />
        ))}
        <ErrorNote>{error}</ErrorNote>
        {result && (
          <div className="rounded-lg border border-success/30 bg-success/5 p-3 text-sm">
            <p className="font-medium">{result.message}</p>
            {result.href && <a className="mt-1 inline-block underline" href={result.href} target="_blank" rel="noreferrer">Open result</a>}
            {(result.notes ?? []).map((note, index) => <p key={index} className="mt-1 text-muted">{note}</p>)}
          </div>
        )}
        <div className="flex justify-end">
          <Button variant="primary" type="submit" busy={busy}>{action.title}</Button>
        </div>
      </form>
    </Card>
  );
}
