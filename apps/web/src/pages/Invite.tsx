import { useState } from "react";
import { api, errorMessage } from "../api/client";
import { Button, ErrorNote } from "../components/ui";
import { useLoad } from "../lib/data";

export default function InvitePage({
  code,
  onJoined,
}: {
  code: string;
  onJoined: (groupId: string) => Promise<void>;
}) {
  const inv = useLoad(
    () => api.GET("/v1/invites/{code}", { params: { path: { code } } }),
    [code],
  );
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  return (
    <div className="space-y-4 rounded-lg border border-slate-800 bg-slate-900/60 p-6">
      {inv.error ? (
        <ErrorNote>This invite link is invalid or has expired.</ErrorNote>
      ) : !inv.data ? (
        <p className="text-sm text-slate-400">Loading invite…</p>
      ) : (
        <>
          <h1 className="text-lg font-semibold">Join {inv.data.group_name}?</h1>
          <p className="text-sm text-slate-400">
            You'll be able to play the group's servers and add your own
            machines.
          </p>
          <ErrorNote>{err}</ErrorNote>
          <Button
            variant="primary"
            className="w-full"
            busy={busy}
            onClick={async () => {
              setBusy(true);
              const { data, error } = await api.POST(
                "/v1/invites/{code}/accept",
                {
                  params: { path: { code } },
                },
              );
              setBusy(false);
              if (error || !data) setErr(errorMessage(error));
              else await onJoined(data.group_id);
            }}
          >
            Join group
          </Button>
        </>
      )}
    </div>
  );
}
