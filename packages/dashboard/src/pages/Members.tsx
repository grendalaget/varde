import { useState } from "react";
import { api, errorMessage } from "../api/client";
import {
  Avatar,
  Badge,
  Button,
  Card,
  CardHeader,
  CopyText,
  ErrorNote,
  PageHeader,
} from "../components/ui";
import { useLoad } from "../lib/data";
import { navigate } from "../lib/router";
import { canAdmin, useSession } from "../lib/session";
import { INVITE_TERMS, useInvite } from "../lib/invite";

export default function Members() {
  const { group, me, refreshMe } = useSession();
  const admin = canAdmin(group.role);
  const owner = group.role === "owner";
  const inv = useInvite(group.group_id);
  const [err, setErr] = useState<string | null>(null);
  const members = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/members", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );

  const remove = async (userId: string) => {
    setErr(null);
    const { error } = await api.DELETE(
      "/v1/groups/{groupId}/members/{userId}",
      { params: { path: { groupId: group.group_id, userId } } },
    );
    if (error) {
      setErr(errorMessage(error));
      return false;
    }
    return true;
  };

  return (
    <>
      <PageHeader
        title="Members"
        subtitle="Everyone in the group can see and play its servers."
        actions={
          admin && (
            <Button
              variant="primary"
              busy={inv.busy}
              onClick={() => void inv.create()}
            >
              Invite friends
            </Button>
          )
        }
      />
      {inv.link && (
        <Card className="vd-pop mb-4">
          <CardHeader title="Invite link" subtitle={INVITE_TERMS} />
          <div className="p-4">
            <CopyText text={inv.link} />
          </div>
        </Card>
      )}
      <ErrorNote>
        {err ??
          inv.error ??
          (members.error ? errorMessage(members.error) : null)}
      </ErrorNote>
      <Card>
        <ul className="vd-stagger divide-y divide-separator">
          {!members.loading &&
            !members.error &&
            !members.data?.members?.length && (
              <li className="p-5 text-sm text-muted">No members yet.</li>
            )}
          {(members.data?.members ?? []).map((m) => (
            <li
              key={m.user_id}
              className="member-row flex flex-wrap items-center justify-between gap-4 px-5 py-5"
            >
              <div className="flex min-w-0 items-center gap-3">
                <Avatar
                  name={m.display_name}
                  className="size-9 rounded-full text-xs"
                />
                <div className="min-w-0">
                  <h2 className="vd-section-title truncate text-foreground">
                    {m.display_name}
                    {m.user_id === me.user.id && (
                      <span className="text-muted"> (you)</span>
                    )}
                  </h2>
                  <div className="truncate text-xs text-muted">{m.email}</div>
                </div>
              </div>
              <div className="flex items-center gap-2">
                {/* admins manage non-owners; only owners manage owners */}
                {admin &&
                m.user_id !== me.user.id &&
                (m.role !== "owner" || owner) ? (
                  <>
                    <select
                      aria-label={`Role for ${m.display_name}`}
                      className="rounded-field border border-field-border bg-field px-3 py-1 text-sm text-field-foreground shadow-field transition-colors duration-150 hover:bg-field-hover"
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
                              role: e.target.value as
                                "owner" | "admin" | "member",
                            },
                          },
                        );
                        if (error) setErr(errorMessage(error));
                        void members.refresh();
                      }}
                    >
                      {owner && <option value="owner">Owner</option>}
                      <option value="admin">Admin</option>
                      <option value="member">Member</option>
                    </select>
                    <Button
                      variant="danger"
                      onClick={async () => {
                        if (
                          !confirm(`Remove ${m.display_name} from the group?`)
                        )
                          return;
                        await remove(m.user_id);
                        void members.refresh();
                      }}
                    >
                      Remove
                    </Button>
                  </>
                ) : (
                  <>
                    <Badge tone={m.role === "owner" ? "violet" : "neutral"}>
                      {m.role[0].toUpperCase() + m.role.slice(1)}
                    </Badge>
                    {m.user_id === me.user.id && (
                      <Button
                        variant="ghost"
                        onClick={async () => {
                          if (!confirm(`Leave ${group.name}?`)) return;
                          if (!(await remove(m.user_id))) return;
                          await refreshMe();
                          navigate("/");
                        }}
                      >
                        Leave group
                      </Button>
                    )}
                  </>
                )}
              </div>
            </li>
          ))}
        </ul>
      </Card>
    </>
  );
}
