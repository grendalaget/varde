import { useState } from "react";
import { api, errorMessage } from "../api/client";
import {
  Button,
  Card,
  CardHeader,
  CopyText,
  ErrorNote,
  Field,
  inputClass,
  PageHeader,
  Toggle,
} from "../components/ui";
import { useLoad } from "../lib/data";
import { dateTime } from "../lib/format";
import { navigate } from "../lib/router";
import { useSession } from "../lib/session";

export default function AddMachine() {
  const { group } = useSession();
  const [code, setCode] = useState("");
  const [anchor, setAnchor] = useState(true);
  const [hosting, setHosting] = useState(false);
  const [token, setToken] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const tokens = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/enrollment-tokens", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );

  async function createToken() {
    setBusy(true);
    setErr(null);
    const { data, error } = await api.POST(
      "/v1/groups/{groupId}/enrollment-tokens",
      {
        params: { path: { groupId: group.group_id } },
        body: {
          anchor,
          hosting_enabled: hosting,
          max_uses: 1,
          expires_at: Date.now() + 24 * 3600 * 1000,
        },
      },
    );
    setBusy(false);
    if (error || !data) setErr(errorMessage(error));
    else {
      setToken(data.token);
      void tokens.refresh();
    }
  }

  return (
    <>
      <PageHeader title="Add a machine" />
      <div className="vd-stagger space-y-6">
        <Card>
          <CardHeader
            title="A gaming PC or laptop"
            subtitle="Install Varde on that PC. Varde shows a short code there and opens this dashboard: enter the code here if it isn't filled in."
          />
          <form
            className="flex gap-2 p-4"
            onSubmit={(e) => {
              e.preventDefault();
              navigate(
                `/link?code=${encodeURIComponent(code.trim().toUpperCase())}`,
              );
            }}
          >
            <input
              className={inputClass + " font-mono uppercase"}
              placeholder="WOLF-73-KITE"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
            <Button variant="primary" type="submit" disabled={!code.trim()}>
              Continue
            </Button>
          </form>
        </Card>
        <Card>
          <CardHeader
            title="A server, NAS or VPS (no screen)"
            subtitle="Create a one-time token and use it in the enroll command on that machine."
          />
          <div className="space-y-4 p-4">
            <div className="flex items-center gap-3 text-sm">
              <Toggle
                checked={anchor}
                onChange={setAnchor}
                label="Always-on backup"
              />
              <span>Always-on backup: keeps a copy of every save</span>
            </div>
            <div className="flex items-center gap-3 text-sm">
              <Toggle
                checked={hosting}
                onChange={setHosting}
                label="Can host"
              />
              <span>Can host servers too</span>
            </div>
            <Button onClick={createToken} busy={busy}>
              Create token
            </Button>
            <ErrorNote>{err}</ErrorNote>
            {token && (
              <div className="vd-pop">
                <Field label="Run this on the machine (the token is shown only once)">
                  <CopyText
                    text={`sudo varde-agent enroll --server ${window.location.origin} --token ${token}`}
                  />
                </Field>
              </div>
            )}
            {(tokens.data?.tokens.length ?? 0) > 0 && (
              <div>
                <h3 className="mb-2 text-xs font-medium uppercase tracking-wider text-skifer-500">
                  Active tokens
                </h3>
                <ul className="vd-stagger divide-y divide-skifer-800 text-sm">
                  {tokens.data!.tokens.map((t) => (
                    <li
                      key={t.id}
                      className="flex items-center justify-between py-2"
                    >
                      <span className="text-skifer-300">
                        {t.anchor ? "Always-on backup" : "Machine"} · used{" "}
                        {t.uses}
                        {t.max_uses ? `/${t.max_uses}` : ""} · expires{" "}
                        {dateTime(t.expires_at)}
                      </span>
                      <Button
                        variant="ghost"
                        onClick={async () => {
                          await api.DELETE(
                            "/v1/groups/{groupId}/enrollment-tokens/{tokenId}",
                            {
                              params: {
                                path: {
                                  groupId: group.group_id,
                                  tokenId: t.id,
                                },
                              },
                            },
                          );
                          void tokens.refresh();
                        }}
                      >
                        Revoke
                      </Button>
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        </Card>
      </div>
    </>
  );
}
