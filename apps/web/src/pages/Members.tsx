import { useState } from "react";
import { api, errorMessage } from "../api/client";
import {
  Badge,
  Button,
  Card,
  CardHeader,
  CopyText,
  ErrorNote,
  PageHeader,
} from "../components/ui";
import { useLoad } from "../lib/data";
import { canAdmin, useSession } from "../lib/session";

export default function Members() {
  const { group, me } = useSession();
  const admin = canAdmin(group.role);
  const [invite, setInvite] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const members = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/members", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );

  return (
    <>
      <PageHeader
        title="Members"
        subtitle="Everyone in the group can see and play its servers."
        actions={
          admin && (
            <Button
              variant="primary"
              onClick={async () => {
                setErr(null);
                const { data, error } = await api.POST(
                  "/v1/groups/{groupId}/invites",
                  {
                    params: { path: { groupId: group.group_id } },
                    body: {
                      role: "member",
                      expires_at: Date.now() + 7 * 24 * 3600 * 1000,
                      max_uses: 10,
                    },
                  },
                );
                if (error || !data) setErr(errorMessage(error));
                else setInvite(`${window.location.origin}/invite/${data.code}`);
              }}
            >
              Invite friends
            </Button>
          )
        }
      />
      {invite && (
        <Card className="vd-pop mb-4">
          <CardHeader
            title="Invite link"
            subtitle="Valid for 7 days, up to 10 people."
          />
          <div className="p-4">
            <CopyText text={invite} />
          </div>
        </Card>
      )}
      <ErrorNote>{err}</ErrorNote>
      <Card>
        <ul className="vd-stagger divide-y divide-skifer-800">
          {(members.data?.members ?? []).map((m) => (
            <li
              key={m.user_id}
              className="flex items-center justify-between gap-3 px-4 py-3"
            >
              <div>
                <div className="font-medium text-skifer-100">
                  {m.display_name}
                  {m.user_id === me.user.id && (
                    <span className="text-skifer-500"> (you)</span>
                  )}
                </div>
                <div className="text-xs text-skifer-400">{m.email}</div>
              </div>
              <div className="flex items-center gap-2">
                {admin && m.role !== "owner" && m.user_id !== me.user.id ? (
                  <>
                    <select
                      className="rounded-md border border-skifer-700 bg-skifer-950 px-2 py-1 text-sm transition-colors duration-150"
                      value={m.role}
                      onChange={async (e) => {
                        const { error } = await api.PATCH(
                          "/v1/groups/{groupId}/members/{userId}",
                          {
                            params: {
                              path: {
                                groupId: group.group_id,
                                userId: m.user_id,
                              },
                            },
                            body: {
                              role: e.target.value as "admin" | "member",
                            },
                          },
                        );
                        if (error) setErr(errorMessage(error));
                        void members.refresh();
                      }}
                    >
                      <option value="admin">Admin</option>
                      <option value="member">Member</option>
                    </select>
                    <Button
                      variant="ghost"
                      onClick={async () => {
                        if (
                          !confirm(`Remove ${m.display_name} from the group?`)
                        )
                          return;
                        const { error } = await api.DELETE(
                          "/v1/groups/{groupId}/members/{userId}",
                          {
                            params: {
                              path: {
                                groupId: group.group_id,
                                userId: m.user_id,
                              },
                            },
                          },
                        );
                        if (error) setErr(errorMessage(error));
                        void members.refresh();
                      }}
                    >
                      Remove
                    </Button>
                  </>
                ) : (
                  <Badge tone={m.role === "owner" ? "violet" : "neutral"}>
                    {m.role}
                  </Badge>
                )}
              </div>
            </li>
          ))}
        </ul>
      </Card>
    </>
  );
}
