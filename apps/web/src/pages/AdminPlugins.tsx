import { useState } from "react";
import { api, errorMessage, type Schemas } from "../api/client";
import { Badge, Button, Card, CardHeader, ErrorNote, PageHeader } from "../components/ui";
import { PageLoader } from "../components/Logo";
import { useLoad } from "../lib/data";
import { useSession } from "../lib/session";

type AdminPlugin = Schemas["AdminPlugin"];
type Action = "install" | "update" | "uninstall";

export default function AdminPlugins() {
  const { me } = useSession();
  const plugins = useLoad(() => api.GET("/v1/admin/plugins"), []);
  const [pending, setPending] = useState<{ id: string; action: Action } | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});

  async function run(plugin: AdminPlugin, action: Action) {
    if (action === "uninstall" && !confirm(`Uninstall ${plugin.name}?`)) return;
    setPending({ id: plugin.id, action });
    setErrors((current) => ({ ...current, [plugin.id]: "" }));
    const result = action === "install"
      ? await api.POST("/v1/admin/plugins/{id}/install", {
        params: { path: { id: plugin.id } },
      })
      : action === "update"
        ? await api.POST("/v1/admin/plugins/{id}/update", {
          params: { path: { id: plugin.id } },
        })
        : await api.DELETE("/v1/admin/plugins/{id}", {
          params: { path: { id: plugin.id } },
        });
    setPending(null);
    if (result.error) {
      setErrors((current) => ({
        ...current,
        [plugin.id]: errorMessage(result.error),
      }));
    } else {
      await plugins.refresh();
    }
  }

  if (!me.operator) {
    return (
      <>
        <PageHeader title="Plugins" />
        <ErrorNote>Operator access is required.</ErrorNote>
      </>
    );
  }
  if (plugins.loading && !plugins.data) return <PageLoader />;

  return (
    <>
      <PageHeader
        title="Plugins"
        subtitle="Install and update plugins for this control plane."
      />
      <ErrorNote>{plugins.error ? errorMessage(plugins.error) : null}</ErrorNote>
      <div className="space-y-4">
        {(plugins.data?.plugins ?? []).map((plugin) => {
          const status = plugin.installed
            ? plugin.status || "Installed"
            : "Not installed";
          return (
            <Card key={plugin.id}>
              <CardHeader
                title={plugin.name}
                subtitle={plugin.description}
                actions={<Badge>{status}</Badge>}
              />
              <div className="space-y-3 p-4">
                <dl className="grid gap-x-4 gap-y-2 text-sm sm:grid-cols-2">
                  <Info label="Installed version">
                    {plugin.installed_version || "Not installed"}
                  </Info>
                  <Info label="Catalog version">{plugin.version}</Info>
                </dl>
                {plugin.error && <ErrorNote>{plugin.error}</ErrorNote>}
                {plugin.upgrade_error && (
                  <ErrorNote>Update failed: {plugin.upgrade_error}</ErrorNote>
                )}
                {errors[plugin.id] && <ErrorNote>{errors[plugin.id]}</ErrorNote>}
                <div className="flex flex-wrap justify-end gap-2">
                  {!plugin.installed ? (
                    <Button
                      variant="primary"
                      disabled={pending !== null}
                      busy={pending?.id === plugin.id && pending.action === "install"}
                      onClick={() => void run(plugin, "install")}
                    >
                      Install
                    </Button>
                  ) : (
                    <>
                      {plugin.update_available && (
                        <Button
                          variant="primary"
                          disabled={pending !== null}
                          busy={pending?.id === plugin.id && pending.action === "update"}
                          onClick={() => void run(plugin, "update")}
                        >
                          Update
                        </Button>
                      )}
                      <Button
                        variant="danger"
                        disabled={pending !== null}
                        busy={pending?.id === plugin.id && pending.action === "uninstall"}
                        onClick={() => void run(plugin, "uninstall")}
                      >
                        Uninstall
                      </Button>
                    </>
                  )}
                </div>
              </div>
            </Card>
          );
        })}
      </div>
    </>
  );
}

function Info({ label, children }: { label: string; children: string }) {
  return (
    <div className="flex justify-between gap-3">
      <dt className="text-muted">{label}</dt>
      <dd className="text-right text-foreground">{children}</dd>
    </div>
  );
}
