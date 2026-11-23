import { useState } from "react";
import type { Server } from "../api/client";
import { usePlatform } from "../lib/platform";
import { Button } from "./ui";

export function JoinCs2Button({ server }: { server: Server }) {
  const { joinCs2Server, thisGroupId } = usePlatform();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const address = server.summary.address;
  if (
    !joinCs2Server ||
    server.group_id !== thisGroupId ||
    server.game_id !== "cs2" ||
    server.desired_state !== "running" ||
    server.observed_state !== "running" ||
    !address
  )
    return null;

  return (
    <div className="space-y-1">
      <Button
        busy={busy}
        onClick={async () => {
          setError(null);
          setBusy(true);
          try {
            await joinCs2Server(address);
          } catch (e) {
            setError(String(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        Join server
      </Button>
      {error && (
        <p className="text-xs text-danger" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
