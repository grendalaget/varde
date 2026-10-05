import { useState } from "react";
import { api, errorMessage } from "../api/client";
import { Button, ErrorNote, Field, inputClass } from "../components/ui";

export default function CreateGroup({
  onCreated,
}: {
  onCreated: (groupId: string) => Promise<void> | void;
}) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  return (
    <form
      className="space-y-4 rounded-lg border border-slate-800 bg-slate-900/60 p-6"
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
        <p className="mt-1 text-sm text-slate-400">
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
  );
}
