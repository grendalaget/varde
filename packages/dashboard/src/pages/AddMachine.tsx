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
import { Icon } from "../components/icons";
import { useLoad } from "../lib/data";
import { dateTime } from "../lib/format";
import { navigate } from "../lib/router";
import { useSession } from "../lib/session";
import { usePlatform } from "../lib/platform";

export default function AddMachine() {
  const { group } = useSession();
  const { cpOrigin, kind, thisNodeId, openExternal } = usePlatform();
  const onLinkedPc = kind === "desktop" && !!thisNodeId;
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
      <PageHeader
        title="Add a machine"
        subtitle={`Add a PC to ${group.name} so you can join your group's servers from it. Hosting is optional: turn Can host on or off for each PC under Machines.`}
      />
      <div className="vd-stagger space-y-6">
        <Card>
          <CardHeader
            icon={<Icon name="machine" className="size-[18px]" />}
            title={onLinkedPc ? "Set up another PC" : "Set up this PC"}
            subtitle={
              onLinkedPc
                ? "This PC is already set up. On the other PC, install Varde and sign in: it shows a short code and asks you to approve it."
                : "Install Varde, open it and sign in. Approve the PC in the browser window it opens, then carry on in the Varde app."
            }
          />
          <div className="flex flex-wrap items-center gap-3 px-4 pt-4">
            <Button
              variant={onLinkedPc || code.trim() ? "secondary" : "primary"}
              onClick={() => openExternal(DOWNLOAD_URL)}
            >
              Download Varde
            </Button>
            <span className="text-xs text-muted">
              For Windows 10 and 11. Other systems are on the download page.
            </span>
          </div>
          <p className="px-4 pt-4 text-sm text-muted">
            Already installed? Enter the code Varde shows on that PC.
          </p>
          <form
            className="flex gap-2 p-4 pt-2"
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
              aria-label="Code shown in Varde"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
            <Button variant={onLinkedPc || code.trim() ? "primary" : "secondary"} type="submit" disabled={!code.trim()}>
              Continue
            </Button>
          </form>
        </Card>
        <details className="group">
          <summary className="cursor-pointer list-none rounded-lg px-1 py-1 text-sm text-muted hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent">
            <span className="underline underline-offset-2">
              Set up another kind of device
            </span>{" "}
            · a server, NAS or VPS without a screen
          </summary>
          <Card className="mt-4">
            <CardHeader
              icon={<Icon name="servers" className="size-[18px]" />}
              title="A server, NAS or VPS (no screen)"
              subtitle="Create a one-time token and use it in the enroll command on that machine."
            />
            <div className="space-y-4 p-4">
              <div className="text-sm">
                <Toggle
                  checked={anchor}
                  onChange={setAnchor}
                  label="Always-on backup"
                >
                  <span>Always-on backup: keeps a copy of every save</span>
                </Toggle>
              </div>
              <div className="text-sm">
                <Toggle
                  checked={hosting}
                  onChange={setHosting}
                  label="Can host"
                >
                  <span>Can host servers too</span>
                </Toggle>
              </div>
              <Button onClick={createToken} busy={busy}>
                Create token
              </Button>
              <ErrorNote>{err}</ErrorNote>
              {token && (
                <div className="vd-pop">
                  <Field label="Run this on the machine (the token is shown only once)">
                    <CopyText
                      text={`sudo varde-agent enroll --server ${cpOrigin} --token ${token}`}
                    />
                  </Field>
                  {kind !== "desktop" && (
                    <p className="mt-3 text-sm text-muted">
                      On the Varde app?{" "}
                      <a
                        className="text-accent underline"
                        href={`varde://enroll?server=${encodeURIComponent(cpOrigin)}&code=${token}`}
                      >
                        Open the app with this token
                      </a>
                    </p>
                  )}
                </div>
              )}
              {(tokens.data?.tokens.length ?? 0) > 0 && (
                <div>
                  <h3 className="mb-2 text-xs font-medium uppercase tracking-wider text-muted">
                    Active tokens
                  </h3>
                  <ul className="vd-stagger divide-y divide-separator text-sm">
                    {tokens.data!.tokens.map((t) => (
                      <li
                        key={t.id}
                        className="flex items-center justify-between py-2"
                      >
                        <span className="text-foreground">
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
        </details>
      </div>
    </>
  );
}

const DOWNLOAD_URL = "https://varde.games/download";
