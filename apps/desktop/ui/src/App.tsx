// Link / status window. Talks only to the tray process (typed commands),
// which talks to the local Varde service.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  Field,
  Input,
  MessageBar,
  MessageBarBody,
  MessageBarTitle,
  Text,
  Title2,
  makeStyles,
  tokens,
} from "@fluentui/react-components";
import {
  Board20Regular,
  Copy20Regular,
  Dismiss20Regular,
  Open20Regular,
  Power20Regular,
} from "@fluentui/react-icons";
import { invoke, listen, type LinkDefaults, type UiStatus } from "./tauri";
import markUrl from "./assets/mark.svg";

const LINKED = ["connecting", "online", "offline"];
const brandFont = "'Schibsted Grotesk', 'Segoe UI', sans-serif";

const useStyles = makeStyles({
  page: {
    boxSizing: "border-box",
    height: "100%",
    padding: "24px 28px",
    backgroundColor: tokens.colorNeutralBackground1,
    color: tokens.colorNeutralForeground1,
    overflowY: "auto",
  },
  header: {
    display: "flex",
    alignItems: "center",
    gap: "10px",
    marginBottom: "28px",
  },
  mark: { width: "28px", height: "28px" },
  brand: {
    fontFamily: brandFont,
    fontSize: "20px",
    fontWeight: 600,
    letterSpacing: "-0.012em",
  },
  title: {
    display: "block",
    fontFamily: brandFont,
    marginTop: 0,
    marginBottom: "10px",
  },
  lead: { display: "block", marginBottom: "14px" },
  muted: {
    display: "block",
    color: tokens.colorNeutralForeground3,
    marginBottom: "14px",
  },
  bar: { marginBottom: "14px" },
  code: {
    fontFamily: "Consolas, ui-monospace, monospace",
    fontSize: "34px",
    fontWeight: 600,
    lineHeight: 1.2,
    letterSpacing: "0.06em",
    backgroundColor: tokens.colorNeutralBackground2,
    border: `1px solid ${tokens.colorNeutralStroke1}`,
    borderRadius: "8px",
    padding: "18px 12px",
    textAlign: "center",
    marginTop: "6px",
    marginBottom: "12px",
    userSelect: "all",
  },
  row: {
    display: "flex",
    gap: "8px",
    flexWrap: "wrap",
    marginTop: "8px",
  },
  form: {
    marginTop: "14px",
    display: "flex",
    flexDirection: "column",
    gap: "14px",
  },
});

type View = "wait" | "form" | "code" | "done" | "linked" | "noagent";

function describe(s: UiStatus) {
  if (s.group_name && s.node_name)
    return `Linked to ${s.group_name} as ${s.node_name}.`;
  if (s.group_name) return `Linked to ${s.group_name}.`;
  return "Linked. Connecting to Varde…";
}

export default function App() {
  const styles = useStyles();
  const [defaults, setDefaults] = useState<LinkDefaults | null>(null);
  const [status, setStatus] = useState<UiStatus | null>(null);
  const [seen, setSeen] = useState(false);
  const [graceDone, setGraceDone] = useState(false);
  const [started, setStarted] = useState(false);
  const [autoPending, setAutoPending] = useState(false);
  const [forceForm, setForceForm] = useState(false);
  const [url, setUrl] = useState("");
  const [urlEdited, setUrlEdited] = useState(false);
  // read by stable callbacks; state mirror above is for render
  const urlEditedRef = useRef(false);
  const [code, setCode] = useState("");
  const [now, setNow] = useState(() => Date.now());
  const [formError, setFormError] = useState("");
  const [codeError, setCodeError] = useState("");
  const [linkedError, setLinkedError] = useState("");
  const [noagentError, setNoagentError] = useState("");
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [linkBusy, setLinkBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const closeTimer = useRef<number | undefined>(undefined);

  // stable (reads the ref): a re-subscribing effect could otherwise race
  // cleanup and leave a stale link-defaults listener that treats the
  // address as unedited and overwrites the user's edit
  const applyDefaults = useCallback((d: LinkDefaults) => {
    setDefaults(d);
    if (!urlEditedRef.current) setUrl(d.url);
    if (d.auto) setAutoPending(true);
  }, []);

  // boot: defaults, status stream, current status
  useEffect(() => {
    let offStatus: (() => void) | undefined;
    let offDefaults: (() => void) | undefined;
    (async () => {
      const d = await invoke<LinkDefaults>("link_defaults").catch(() => null);
      if (d) applyDefaults(d);
      offStatus = await listen<UiStatus | null>("status", (e) =>
        setStatus(e.payload),
      );
      // the tray was started again with --link while this window was open
      offDefaults = await listen<LinkDefaults>("link-defaults", (e) =>
        applyDefaults(e.payload),
      );
      setStatus(await invoke<UiStatus | null>("get_status").catch(() => null));
    })();
    return () => {
      offStatus?.();
      offDefaults?.();
    };
  }, [applyDefaults]);

  // the tray connects within moments of starting; wait briefly before
  // calling the service dead
  useEffect(() => {
    const t = setTimeout(() => setGraceDone(true), 3000);
    return () => clearTimeout(t);
  }, []);

  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);

  const formState =
    !!status &&
    (status.state === "not_linked" ||
      (!!defaults?.relink && LINKED.includes(status.state)));

  // side effects that follow a new status
  useEffect(() => {
    const s = status;
    if (!s) return;
    setSeen(true);
    setForceForm(false);
    if (s.state === "linking" && s.user_code) {
      setStarted(true);
      // a new code: drop a refusal shown for the previous one
      if (s.user_code !== code) setCodeError("");
      setCode(s.user_code);
    }
    if (!urlEditedRef.current && s.control_plane_url)
      setUrl(s.control_plane_url);
    if (formState) setFormError(s.link_error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status]);

  const view: View = useMemo(() => {
    const s = status;
    if (!s) return seen || graceDone ? "noagent" : "wait";
    if (s.state === "linking" && s.user_code) return "code";
    if (started && LINKED.includes(s.state)) return "done";
    if (forceForm || formState) return "form";
    if (LINKED.includes(s.state)) return "linked";
    return "noagent";
  }, [status, seen, graceDone, started, forceForm, formState]);

  // auto-close the "Linked" view on the transition INTO online — status
  // still says "online" while a re-link is being set up (and after Cancel),
  // so arming on any done+online render would self-close under a pending
  // link; keying on a code→done view transition misses because approval
  // emits connecting (LINKED) before online
  const prevState = useRef<string | undefined>(undefined);
  useEffect(() => {
    const was = prevState.current;
    prevState.current = status?.state;
    if (view === "done" && status?.state === "online") {
      if (
        was !== undefined &&
        was !== "online" &&
        closeTimer.current === undefined
      ) {
        closeTimer.current = window.setTimeout(
          () => void invoke("close_window"),
          4000,
        );
      }
    } else if (closeTimer.current !== undefined) {
      clearTimeout(closeTimer.current);
      closeTimer.current = undefined;
    }
  }, [view, status]);

  const effectiveUrl =
    urlEdited || url ? url : status?.control_plane_url || defaults?.url || "";

  const doLink = useCallback(async (u: string) => {
    setFormError("");
    setLinkBusy(true);
    try {
      setStatus(await invoke<UiStatus>("start_link", { url: u }));
    } catch (e) {
      setStarted(false);
      setFormError(String(e));
    } finally {
      setLinkBusy(false);
    }
  }, []);

  // installer launch (--link): start linking as soon as not_linked shows
  useEffect(() => {
    if (view === "form" && autoPending && status?.state === "not_linked") {
      setAutoPending(false);
      void doLink(effectiveUrl);
    }
  }, [view, autoPending, status, effectiveUrl, doLink]);

  const service = useCallback(
    async (
      cmd: "start_service" | "stop_service",
      key: string,
      setErr: (m: string) => void,
    ) => {
      setErr("");
      setBusy((b) => ({ ...b, [key]: true }));
      try {
        // the elevated instance does the work; status arrives on its own
        await invoke(cmd);
        setTimeout(() => setBusy((b) => ({ ...b, [key]: false })), 4000);
      } catch (e) {
        setBusy((b) => ({ ...b, [key]: false }));
        setErr(String(e));
      }
    },
    [],
  );

  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(code);
    } catch {
      const r = document.createRange();
      const el = document.getElementById("link-code");
      if (el) {
        r.selectNodeContents(el);
        getSelection()?.removeAllRanges();
        getSelection()?.addRange(r);
        document.execCommand("copy");
      }
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  }, [code]);

  const cancelLink = useCallback(async () => {
    setCodeError("");
    try {
      await invoke("cancel_link");
    } catch (e) {
      setCodeError(String(e));
      return;
    }
    setStarted(false);
    setForceForm(true);
  }, []);

  const expiry = (() => {
    if (!status || status.state !== "linking" || !status.expires_at_unix_ms)
      return "";
    const left = Math.max(
      0,
      Math.round((status.expires_at_unix_ms - now) / 1000),
    );
    const m = Math.floor(left / 60);
    const sec = String(left % 60).padStart(2, "0");
    return `Code expires in ${m}:${sec}`;
  })();

  // only the elevated --relink window can cancel an administrator's re-link
  const locked = !!status?.relink && !defaults?.relink;

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <img src={markUrl} alt="" className={styles.mark} />
        <span className={styles.brand}>varde</span>
      </header>
      <main>
        {view === "form" && (
          <section>
            <Title2 className={styles.title} as="h1">
              {defaults?.relink ? "Re-link this PC" : "Link this PC"}
            </Title2>
            <Text className={styles.lead} as="p">
              Enter your Varde address. You'll approve this PC in your browser.
            </Text>
            {defaults?.relink && (
              <MessageBar intent="warning" className={styles.bar}>
                <MessageBarBody>
                  This PC leaves its current group and gets a new identity.
                </MessageBarBody>
              </MessageBar>
            )}
            <form
              className={styles.form}
              onSubmit={(e) => {
                e.preventDefault();
                void doLink(effectiveUrl.trim());
              }}
            >
              <Field label="Varde address" required>
                <Input
                  type="url"
                  required
                  spellCheck={false}
                  autoComplete="url"
                  value={url}
                  onChange={(_, d) => {
                    urlEditedRef.current = true;
                    setUrl(d.value);
                    setUrlEdited(true);
                  }}
                />
              </Field>
              {formError && (
                <MessageBar intent="error" className={styles.bar}>
                  <MessageBarBody>{formError}</MessageBarBody>
                </MessageBar>
              )}
              <div>
                <Button appearance="primary" type="submit" disabled={linkBusy}>
                  Link
                </Button>
              </div>
            </form>
          </section>
        )}

        {view === "code" && status && (
          <section>
            <Title2 className={styles.title} as="h1">
              Approve this PC in your browser
            </Title2>
            <Text className={styles.lead} as="p">
              Your browser opened Varde. Check that it shows this code, then
              approve:
            </Text>
            <div id="link-code" className={styles.code} aria-live="polite">
              {code}
            </div>
            <Text className={styles.muted} as="p">
              {expiry}
            </Text>
            {locked && (
              <Text className={styles.muted} as="p">
                An administrator is re-linking this PC.
              </Text>
            )}
            {codeError && (
              <MessageBar intent="error" className={styles.bar}>
                <MessageBarBody>{codeError}</MessageBarBody>
              </MessageBar>
            )}
            <div className={styles.row}>
              <Button icon={<Copy20Regular />} onClick={() => void copy()}>
                {copied ? "Copied" : "Copy code"}
              </Button>
              <Button
                icon={<Open20Regular />}
                onClick={() =>
                  void invoke("open_url", { url: status.link_url })
                }
              >
                Open again
              </Button>
              {!locked && (
                <Button
                  icon={<Dismiss20Regular />}
                  onClick={() => void cancelLink()}
                >
                  Cancel
                </Button>
              )}
            </div>
          </section>
        )}

        {view === "done" && status && (
          <section>
            <Title2 className={styles.title} as="h1">
              Linked
            </Title2>
            <Text className={styles.lead} as="p">
              {describe(status)}
            </Text>
            <Text className={styles.muted} as="p">
              The Varde service is running on this PC.
            </Text>
            <div className={styles.row}>
              <Button
                appearance="primary"
                onClick={() => void invoke("close_window")}
              >
                Close
              </Button>
            </div>
          </section>
        )}

        {view === "linked" && status && (
          <section>
            <Title2 className={styles.title} as="h1">
              This PC is linked
            </Title2>
            <Text className={styles.lead} as="p">
              {describe(status)}
            </Text>
            <Text className={styles.muted} as="p">
              The Varde service is running on this PC.
            </Text>
            {linkedError && (
              <MessageBar intent="error" className={styles.bar}>
                <MessageBarBody>{linkedError}</MessageBarBody>
              </MessageBar>
            )}
            <div className={styles.row}>
              <Button
                appearance="primary"
                icon={<Board20Regular />}
                onClick={() => {
                  setLinkedError("");
                  void invoke("open_dashboard").catch((e) =>
                    setLinkedError(String(e)),
                  );
                }}
              >
                Open dashboard
              </Button>
              <Button
                appearance="subtle"
                icon={<Open20Regular />}
                onClick={() =>
                  void invoke("open_url", { url: status.control_plane_url })
                }
              >
                Open in browser
              </Button>
              {/* stopping while hosting would leave the game running unmanaged */}
              {!status.hosting && (
                <Button
                  icon={<Power20Regular />}
                  disabled={busy["svc-stop"]}
                  onClick={() =>
                    void service("stop_service", "svc-stop", setLinkedError)
                  }
                >
                  {busy["svc-stop"] ? "Stopping…" : "Stop Varde"}
                </Button>
              )}
              <Button
                icon={<Dismiss20Regular />}
                onClick={() => void invoke("close_window")}
              >
                Close
              </Button>
            </div>
          </section>
        )}

        {view === "noagent" && (
          <section>
            <Title2 className={styles.title} as="h1">
              Varde isn't running
            </Title2>
            <MessageBar intent="warning" className={styles.bar}>
              <MessageBarBody>
                <MessageBarTitle>Service unreachable</MessageBarTitle>
                The Varde service on this PC isn't answering. Varde can't host
                or join servers until it runs again.
              </MessageBarBody>
            </MessageBar>
            <Text className={styles.muted} as="p">
              It normally starts with Windows. Starting it needs administrator
              approval once.
            </Text>
            {noagentError && (
              <MessageBar intent="error" className={styles.bar}>
                <MessageBarBody>{noagentError}</MessageBarBody>
              </MessageBar>
            )}
            <div className={styles.row}>
              <Button
                appearance="primary"
                icon={<Power20Regular />}
                disabled={busy["svc-start"]}
                onClick={() =>
                  void service("start_service", "svc-start", setNoagentError)
                }
              >
                {busy["svc-start"] ? "Starting…" : "Start Varde"}
              </Button>
              <Button
                icon={<Dismiss20Regular />}
                onClick={() => void invoke("close_window")}
              >
                Close
              </Button>
            </div>
          </section>
        )}
      </main>
    </div>
  );
}
