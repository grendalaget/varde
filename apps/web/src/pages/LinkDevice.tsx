import { useEffect, useState } from "react";
import { api, errorMessage } from "../api/client";
import {
  Button,
  Card,
  CardHeader,
  ErrorNote,
  Field,
  inputClass,
  PageHeader,
} from "../components/ui";
import { useLoad } from "../lib/data";
import { navigate } from "../lib/router";
import { canAdmin, useSession } from "../lib/session";

export default function LinkDevice({ code }: { code: string }) {
  const { me, group, selectGroup, refreshNodes } = useSession();
  const adminGroups = me.groups.filter((g) => canAdmin(g.role));
  const [groupId, setGroupId] = useState(
    canAdmin(group.role) ? group.group_id : (adminGroups[0]?.group_id ?? ""),
  );
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const link = useLoad(
    () =>
      api.GET("/v1/device-links/{userCode}", {
        params: { path: { userCode: code } },
      }),
    [code],
  );
  useEffect(() => {
    if (link.data && !name) setName(link.data.hostname);
  }, [link.data, name]);

  const d = link.data;
  return (
    <>
      <PageHeader title="Add this machine?" />
      <Card className="max-w-lg">
        <CardHeader
          title={<span className="font-mono">{code || "No code"}</span>}
        />
        <div className="space-y-4 p-4">
          {link.error ? (
            <ErrorNote>
              That code wasn't found or has expired. Check the code shown on the
              machine.
            </ErrorNote>
          ) : !d ? (
            <p className="text-sm text-slate-400">Looking up code…</p>
          ) : d.state !== "pending" ? (
            <ErrorNote>
              This code was already used or has expired ({d.state}).
            </ErrorNote>
          ) : (
            <form
              className="space-y-4"
              onSubmit={async (e) => {
                e.preventDefault();
                setBusy(true);
                setErr(null);
                const { error } = await api.POST("/v1/device-links/approve", {
                  body: { user_code: code, group_id: groupId, name },
                });
                setBusy(false);
                if (error) setErr(errorMessage(error));
                else {
                  selectGroup(groupId);
                  await refreshNodes();
                  navigate("/machines");
                }
              }}
            >
              <p className="text-sm text-slate-300">
                <span className="font-medium text-slate-100">{d.hostname}</span>{" "}
                ({d.os}/{d.arch}) wants to join. Only approve it if you just
                installed Varde on this machine.
              </p>
              <Field label="Name">
                <input
                  className={inputClass}
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </Field>
              <Field label="Group">
                <select
                  className={inputClass}
                  value={groupId}
                  onChange={(e) => setGroupId(e.target.value)}
                >
                  {adminGroups.map((g) => (
                    <option key={g.group_id} value={g.group_id}>
                      {g.name}
                    </option>
                  ))}
                </select>
              </Field>
              <ErrorNote>{err}</ErrorNote>
              <Button
                variant="primary"
                type="submit"
                busy={busy}
                disabled={!groupId}
              >
                Add machine
              </Button>
            </form>
          )}
        </div>
      </Card>
    </>
  );
}
