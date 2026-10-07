import { useState } from "react";
import { api, errorMessage } from "../api/client";
import { Button, ErrorNote, Field, inputClass } from "../components/ui";
import { navigate } from "../lib/router";

/** Accepts a bare code or a pasted invite link. */
function inviteCode(raw: string): string {
  const t = raw.trim().split(/[?#]/)[0];
  return t.split("/").filter(Boolean).pop() ?? "";
}

export default function CreateGroup({
  onCreated,
}: {
  onCreated: (groupId: string) => Promise<void> | void;
}) {
  const [name, setName] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  return (
    <div className="space-y-4">
      <form
        className="space-y-4 rounded-lg border border-skifer-800 bg-skifer-900/60 p-6"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          const { data, error } = await api.POST("/v1/groups", {
            body: { name },
          });
          setBusy(false);
          if (error || !data) setErr(errorMessage(error));
          else await onCreated(data.id);
        }}
      >
        <div>
          <h1 className="text-lg font-semibold">Create your group</h1>
          <p className="mt-1 text-sm text-skifer-400">
            A group is you and your friends. Machines and servers belong to the
            group, so anyone's PC can host.
          </p>
        </div>
        <Field label="Group name">
          <input
            className={inputClass}
            required
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. Friday crew"
          />
        </Field>
        <ErrorNote>{err}</ErrorNote>
        <Button variant="primary" type="submit" busy={busy} className="w-full">
          Create group
        </Button>
      </form>
      <form
        className="space-y-4 rounded-lg border border-skifer-800 bg-skifer-900/60 p-6"
        onSubmit={(e) => {
          e.preventDefault();
          const c = inviteCode(code);
          if (c) navigate(`/invite/${encodeURIComponent(c)}`);
        }}
      >
        <div>
          <h2 className="text-lg font-semibold">Join your friends' group</h2>
          <p className="mt-1 text-sm text-skifer-400">
            Someone invited you? Paste the invite link or its code.
          </p>
        </div>
        <Field label="Invite link or code">
          <input
            className={inputClass}
            required
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder="e.g. …/invite/x7k2m9 or x7k2m9"
          />
        </Field>
        <Button type="submit" className="w-full">
          Join group
        </Button>
      </form>
    </div>
  );
}
