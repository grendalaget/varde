import { useState } from "react";
import { api, errorMessage } from "../api/client";
import { usePlatform } from "./platform";

export const INVITE_TERMS =
  "Anyone with this link can join the group as a member: they can play on, start, stop and move its servers. It works for 7 days, for up to 10 people, so share it only with friends.";

/** Creates a group invite link (admin+): 7 days, up to 10 people. */
export function useInvite(groupId: string) {
  const { cpOrigin } = usePlatform();
  const [link, setLink] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  async function create() {
    setBusy(true);
    setError(null);
    const { data, error } = await api.POST("/v1/groups/{groupId}/invites", {
      params: { path: { groupId } },
      body: {
        role: "member",
        expires_at: Date.now() + 7 * 24 * 3600 * 1000,
        max_uses: 10,
      },
    });
    setBusy(false);
    if (error || !data) setError(errorMessage(error));
    else setLink(`${cpOrigin}/invite/${data.code}`);
  }
  return { link, error, busy, create };
}
