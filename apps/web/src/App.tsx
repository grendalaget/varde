import { useCallback, useEffect, useMemo, useState } from "react";
import { api, errorMessage, type Me, type Node } from "./api/client";
import { Logo, PageLoader } from "./components/Logo";
import { Button, cx, Dot, ErrorNote } from "./components/ui";
import { GroupEventsProvider, useGroupEvents, useOnEvent } from "./lib/events";
import { Link, match, navigate, useLocation } from "./lib/router";
import { SessionProvider, useSession } from "./lib/session";
import Activity from "./pages/Activity";
import AddMachine from "./pages/AddMachine";
import CreateGroup from "./pages/CreateGroup";
import InvitePage from "./pages/Invite";
import LinkDevice from "./pages/LinkDevice";
import Login from "./pages/Login";
import Machines from "./pages/Machines";
import Members from "./pages/Members";
import NewServer from "./pages/NewServer";
import ServerDetail from "./pages/ServerDetail";
import Servers from "./pages/Servers";
import Settings from "./pages/Settings";
import PluginPage from "./pages/PluginPage";
import { useLoad } from "./lib/data";

const GROUP_KEY = "varde.group";
// The site's federated sign-in bounce mints a token and hands it back in the
// URL fragment. Kept in sync with auth.FederationIssuer on the CP.
const VARDE_SITE = "https://varde.games";
// Set once per tab so a failed exchange can't loop the bounce forever.
const FED_AUTO_KEY = "varde.fed-auto";
// Set by an explicit sign-out: a domain-wide site cookie would otherwise
// bounce the user straight back into a fresh CP session.
const SIGNED_OUT_KEY = "varde.signed-out";
// Where to return after the federated bounce. Kept in tab storage on this
// origin: putting it in `next` would hand paths like /invite/<code> to the
// site's logs. Stored as `path|timestamp` — a stale value (sign-in abandoned
// at the site) must not hijack a later, unrelated exchange.
const FED_RETURN_KEY = "varde.fed-return";
const FED_RETURN_TTL_MS = 15 * 60 * 1000;

export default function App() {
  const [me, setMe] = useState<Me | null | undefined>(undefined);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [fedError, setFedError] = useState<string | null>(null);
  const [fed, setFed] = useState<{
    enabled: boolean;
    site_session: boolean;
  } | null>(null);
  const [groupId, setGroupId] = useState<string | null>(() =>
    localStorage.getItem(GROUP_KEY),
  );
  const loc = useLocation();

  const refreshMe = useCallback(async () => {
    const { data, error, response } = await api.GET("/v1/me");
    if (response?.status === 401) {
      setMe(null);
      return;
    }
    if (error || !data) {
      setLoadError(errorMessage(error));
      return;
    }
    setLoadError(null);
    setMe(data);
  }, []);

  // The site hands the federation token back in the fragment so it never
  // reaches server logs or referers; exchange it for a CP session.
  const exchangeFedToken = useCallback(async () => {
    const m = window.location.hash.match(/^#fed_token=(.+)$/);
    if (!m) return;
    history.replaceState(
      null,
      "",
      window.location.pathname + window.location.search,
    );
    const token = decodeURIComponent(m[1]);
    const { error } = await api.POST("/v1/auth/external", {
      body: { token },
    });
    if (error) setFedError(errorMessage(error));
    await refreshMe();
    // Back from the site's bounce: return to the page that sent us there.
    const back = sessionStorage.getItem(FED_RETURN_KEY);
    sessionStorage.removeItem(FED_RETURN_KEY);
    const [path, ts] = (back ?? "").split("|");
    if (
      path.startsWith("/") &&
      Date.now() - Number(ts) < FED_RETURN_TTL_MS &&
      path !== window.location.pathname + window.location.search
    ) {
      navigate(path, true);
    }
  }, [refreshMe]);

  useEffect(() => {
    if (window.location.hash.startsWith("#fed_token=")) {
      void exchangeFedToken();
    } else {
      // mounted without a token: the bounce chain is over — any stored
      // return path belongs to an abandoned sign-in
      sessionStorage.removeItem(FED_RETURN_KEY);
      void refreshMe();
    }
  }, [exchangeFedToken, refreshMe]);

  // A token pasted into an already-open tab is a fragment-only navigation -
  // no remount, so catch it via hashchange too.
  useEffect(() => {
    const onHash = () => void exchangeFedToken();
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, [exchangeFedToken]);

  const continueWithVarde = useCallback(() => {
    sessionStorage.removeItem(SIGNED_OUT_KEY);
    // Preserve the current path (e.g. /invite/<code>) across the bounce —
    // locally, so invite codes never reach the site's query logs.
    sessionStorage.setItem(
      FED_RETURN_KEY,
      `${window.location.pathname + window.location.search}|${Date.now()}`,
    );
    window.location.assign(`${VARDE_SITE}/federate/cp?next=/`);
  }, []);

  // Logged out on a federated CP: if the browser already has a domain-wide
  // varde.games session, bounce to the site once - it mints a token and
  // returns it in the fragment, so no second login is needed.
  useEffect(() => {
    if (me !== null) return;
    let dead = false;
    fetch("/v1/federated/state", { credentials: "same-origin" })
      .then((r) => (r.ok ? r.json() : null))
      .then((f) => {
        if (dead || !f) return;
        setFed(f);
        if (
          f.enabled &&
          f.site_session &&
          !fedError &&
          !sessionStorage.getItem(FED_AUTO_KEY) &&
          !sessionStorage.getItem(SIGNED_OUT_KEY)
        ) {
          sessionStorage.setItem(FED_AUTO_KEY, "1");
          continueWithVarde();
        }
      })
      .catch(() => {});
    return () => {
      dead = true;
    };
  }, [me, fedError, continueWithVarde]);

  const selectGroup = useCallback((id: string) => {
    localStorage.setItem(GROUP_KEY, id);
    setGroupId(id);
  }, []);

  if (me === undefined) {
    return (
      <Centered>
        {loadError ? (
          <ErrorNote>Can't reach the control plane: {loadError}</ErrorNote>
        ) : (
          <PageLoader />
        )}
      </Centered>
    );
  }

  if (me === null)
    return (
      <Login
        onAuthed={refreshMe}
        federated={fed?.enabled}
        initialError={fedError}
        onContinue={continueWithVarde}
      />
    );

  // Signed in but a federated link failed (e.g. wrong-email account) —
  // the Login surface is gone, so show it here instead of silently dropping.
  if (fedError) {
    return (
      <Centered>
        <ErrorNote>{fedError}</ErrorNote>
        <Button variant="secondary" onClick={() => setFedError(null)}>
          Dismiss
        </Button>
      </Centered>
    );
  }

  const inviteParams = match("/invite/:code", loc.pathname);
  if (inviteParams) {
    return (
      <Centered>
        <InvitePage
          code={inviteParams.code}
          onJoined={async (gid) => {
            await refreshMe();
            selectGroup(gid);
            navigate("/", true);
          }}
        />
      </Centered>
    );
  }

  if (me.groups.length === 0) {
    return (
      <Centered>
        <CreateGroup
          onCreated={async (gid) => {
            await refreshMe();
            selectGroup(gid);
          }}
        />
      </Centered>
    );
  }

  const group = me.groups.find((g) => g.group_id === groupId) ?? me.groups[0];

  return (
    <GroupEventsProvider groupId={group.group_id}>
      <GroupShell
        me={me}
        group={group}
        refreshMe={refreshMe}
        selectGroup={selectGroup}
      />
    </GroupEventsProvider>
  );
}

function GroupShell({
  me,
  group,
  refreshMe,
  selectGroup,
}: {
  me: Me;
  group: Me["groups"][number];
  refreshMe: () => Promise<void>;
  selectGroup: (id: string) => void;
}) {
  const [nodes, setNodes] = useState<Node[]>([]);
  const refreshNodes = useCallback(async () => {
    const { data } = await api.GET("/v1/groups/{groupId}/nodes", {
      params: { path: { groupId: group.group_id } },
    });
    if (data) setNodes(data.nodes);
  }, [group.group_id]);

  useEffect(() => {
    setNodes([]);
    void refreshNodes();
    const t = setInterval(() => void refreshNodes(), 15000);
    return () => clearInterval(t);
  }, [refreshNodes]);

  useOnEvent((e) => e.type.startsWith("node."), refreshNodes);

  const session = useMemo(
    () => ({ me, group, refreshMe, selectGroup, nodes, refreshNodes }),
    [me, group, refreshMe, selectGroup, nodes, refreshNodes],
  );

  return (
    <SessionProvider value={session}>
      <Layout />
    </SessionProvider>
  );
}

const NAV = [
  {
    to: "/",
    label: "Servers",
    active: (p: string) => p === "/" || p.startsWith("/servers"),
  },
  {
    to: "/machines",
    label: "Machines",
    active: (p: string) => p.startsWith("/machines"),
  },
  {
    to: "/members",
    label: "Members",
    active: (p: string) => p.startsWith("/members"),
  },
  {
    to: "/activity",
    label: "Activity",
    active: (p: string) => p.startsWith("/activity"),
  },
  {
    to: "/settings",
    label: "Settings",
    active: (p: string) => p.startsWith("/settings"),
  },
];

function Layout() {
  const { me, group, selectGroup } = useSession();
  const { connected } = useGroupEvents();
  const loc = useLocation();
  const p = loc.pathname;
  const pluginCatalog = useLoad(() => api.GET("/v1/plugins"), []);
  const groupPlugins = useLoad(
    () => pluginCatalog.data?.plugins.length
      ? api.GET("/v1/groups/{groupId}/plugins", {
        params: { path: { groupId: group.group_id } },
      })
      : Promise.resolve({ data: { plugins: [] } }),
    [group.group_id, pluginCatalog.data?.plugins.length],
  );
  const refreshGroupPlugins = groupPlugins.refresh;
  useEffect(() => {
    const refreshPlugins = () => void refreshGroupPlugins();
    window.addEventListener("varde:plugins-changed", refreshPlugins);
    return () => window.removeEventListener("varde:plugins-changed", refreshPlugins);
  }, [refreshGroupPlugins]);
  const pluginNav =
    pluginCatalog.data?.plugins
      .filter((plugin) =>
        groupPlugins.data?.plugins.some(
          (enabled) => enabled.id === plugin.id && enabled.enabled,
        ),
      )
      .flatMap((plugin) =>
        plugin.pages.map((page) => ({
          to: `/plugins/${plugin.id}/${page.id}`,
          label: page.title,
          active: (path: string) => path === `/plugins/${plugin.id}/${page.id}`,
        })),
      ) ?? [];
  const nav = [...NAV, ...pluginNav];

  let page;
  let m: Record<string, string> | null;
  if (p === "/" || p === "/servers") page = <Servers />;
  else if (p === "/servers/new") page = <NewServer />;
  else if ((m = match("/servers/:id", p)))
    page = <ServerDetail id={m.id} key={m.id} />;
  else if (p === "/machines") page = <Machines />;
  else if (p === "/machines/add") page = <AddMachine />;
  else if (p === "/link")
    page = <LinkDevice code={loc.search.get("code") ?? ""} />;
  else if (p === "/members") page = <Members />;
  else if (p === "/activity") page = <Activity />;
  else if (p === "/settings") page = <Settings />;
  else if ((m = match("/plugins/:pluginId/:pageId", p)))
    page = <PluginPage pluginId={m.pluginId} pageId={m.pageId} />;
  else page = <p className="text-muted">Page not found.</p>;

  return (
    <Shell
      sidebar={
        <>
          <Link
            to="/"
            className="vd-hover mb-6 hidden px-2 pt-1 md:block"
            aria-label="Varde home"
          >
            <Logo className="h-8" />
          </Link>
          <label className="mb-4 px-2">
            <span className="text-[11px] font-medium uppercase tracking-wider text-muted">
              Group
            </span>
            <select
              className="mt-1 w-full rounded-field border border-field-border bg-field px-2 py-1.5 text-sm text-field-foreground shadow-field"
              value={group.group_id}
              onChange={(e) => {
                if (e.target.value === "__new") navigate("/settings?new=1");
                else {
                  selectGroup(e.target.value);
                  navigate("/");
                }
              }}
            >
              {me.groups.map((g) => (
                <option key={g.group_id} value={g.group_id}>
                  {g.name}
                </option>
              ))}
              <option value="__new">+ New group…</option>
            </select>
          </label>
          <nav className="vd-stagger flex flex-col gap-0.5">
            {nav.map((n) => (
              <Link
                key={n.to}
                to={n.to}
                className={cx(
                  "rounded-md px-2 py-1.5 text-sm transition-all duration-200 ease-vd-out hover:translate-x-0.5",
                  n.active(p)
                    ? "bg-default font-medium text-foreground"
                    : "text-muted hover:bg-surface-secondary hover:text-foreground",
                )}
              >
                {n.label}
              </Link>
            ))}
          </nav>
          <div className="mt-auto space-y-3 px-2 text-xs text-muted">
            <div className="flex items-center gap-2" title="Live updates">
              <span className={connected ? "vd-pulse" : undefined}>
                <Dot tone={connected ? "green" : "warn"} />
              </span>
              {connected ? "Live" : "Reconnecting…"}
            </div>
            <div className="truncate text-muted">{me.user.display_name}</div>
            <button
              className="text-muted transition-colors duration-150 hover:text-foreground"
              onClick={async () => {
                await api.POST("/v1/auth/logout");
                // A domain-wide site cookie would auto-bounce straight back
                // into a fresh session - remember the explicit sign-out.
                sessionStorage.setItem(SIGNED_OUT_KEY, "1");
                window.location.href = "/";
              }}
            >
              Sign out
            </button>
          </div>
        </>
      }
    >
      <div key={p} className="vd-fade">
        {page}
      </div>
    </Shell>
  );
}

function Shell({
  sidebar,
  children,
}: {
  sidebar: React.ReactNode;
  children: React.ReactNode;
}) {
  const { pathname } = useLocation();
  const [open, setOpen] = useState(false);
  const [lastPath, setLastPath] = useState(pathname);
  if (pathname !== lastPath) {
    setLastPath(pathname);
    setOpen(false);
  }
  return (
    <div className="min-h-screen bg-background text-foreground md:flex">
      <header className="sticky top-0 z-30 flex items-center justify-between border-b border-separator bg-background/90 px-4 py-3 backdrop-blur md:hidden">
        <Link to="/" className="vd-hover flex" aria-label="Varde home">
          <Logo className="h-7" />
        </Link>
        <button
          type="button"
          aria-label={open ? "Close menu" : "Open menu"}
          aria-expanded={open}
          onClick={() => setOpen((o) => !o)}
          className="-mr-2 rounded-md p-2 text-foreground transition-colors duration-150 hover:bg-surface-secondary"
        >
          <svg
            width="22"
            height="22"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
          >
            {open ? (
              <path d="M6 6l12 12M18 6L6 18" />
            ) : (
              <path d="M4 7h16M4 12h16M4 17h16" />
            )}
          </svg>
        </button>
      </header>
      {open && (
        <div
          className="vd-fade fixed inset-0 z-40 bg-black/60 md:hidden"
          onClick={() => setOpen(false)}
        />
      )}
      <aside
        className={cx(
          "fixed inset-y-0 left-0 z-50 flex w-64 flex-col overflow-y-auto border-r border-separator bg-surface px-3 py-4 transition-transform duration-300 ease-vd-out md:sticky md:top-0 md:h-screen md:w-56 md:shrink-0 md:translate-x-0",
          open ? "translate-x-0" : "-translate-x-full",
        )}
      >
        {sidebar}
      </aside>
      <main className="min-w-0 flex-1 px-4 py-6 sm:px-6 md:px-8 md:py-8">
        <div className="mx-auto max-w-5xl">{children}</div>
      </main>
    </div>
  );
}

function Centered({ children }: { children: React.ReactNode }) {
  return (
    <main className="flex min-h-screen items-center justify-center bg-background p-6 text-foreground">
      <div className="w-full max-w-sm">{children}</div>
    </main>
  );
}
