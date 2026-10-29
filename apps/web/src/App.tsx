import { useCallback, useEffect, useMemo, useState } from "react";
import { api, errorMessage, type Me, type Node } from "./api/client";
import { Logo, PageLoader } from "./components/Logo";
import { Avatar, Button, cx, Dot, ErrorNote, TopGlow } from "./components/ui";
import { Icon, type IconName } from "./components/icons";
import { GroupEventsProvider, useGroupEvents, useOnEvent } from "./lib/events";
import { Link, match, navigate, useLocation } from "./lib/router";
import { SessionProvider, useSession } from "./lib/session";
import Activity from "./pages/Activity";
import AdminPlugins from "./pages/AdminPlugins";
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
  // The desktop app opens the dashboard on the PC's own group (?group=).
  // Not persisted: switching groups here still sticks, and an id the user
  // isn't in falls back to their first group below.
  const [groupId, setGroupId] = useState<string | null>(
    () =>
      new URLSearchParams(window.location.search).get("group") ??
      localStorage.getItem(GROUP_KEY),
  );
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    if (!params.has("group")) return;
    params.delete("group");
    const q = params.toString();
    history.replaceState(
      null,
      "",
      window.location.pathname + (q ? `?${q}` : "") + window.location.hash,
    );
  }, []);
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
  const [nodesLoaded, setNodesLoaded] = useState(false);
  const refreshNodes = useCallback(async () => {
    const { data } = await api.GET("/v1/groups/{groupId}/nodes", {
      params: { path: { groupId: group.group_id } },
    });
    if (data) {
      setNodes(data.nodes);
      setNodesLoaded(true);
    }
  }, [group.group_id]);

  useEffect(() => {
    setNodes([]);
    setNodesLoaded(false);
    void refreshNodes();
    const t = setInterval(() => void refreshNodes(), 15000);
    return () => clearInterval(t);
  }, [refreshNodes]);

  useOnEvent((e) => e.type.startsWith("node."), refreshNodes);

  const session = useMemo(
    () => ({
      me,
      group,
      refreshMe,
      selectGroup,
      nodes,
      nodesLoaded,
      refreshNodes,
    }),
    [me, group, refreshMe, selectGroup, nodes, nodesLoaded, refreshNodes],
  );

  return (
    <SessionProvider value={session}>
      <Layout />
    </SessionProvider>
  );
}

const NAV: {
  to: string;
  label: string;
  icon: IconName;
  active: (p: string) => boolean;
}[] = [
  {
    to: "/",
    label: "Servers",
    icon: "servers",
    active: (p: string) => p === "/" || p.startsWith("/servers"),
  },
  {
    to: "/machines",
    icon: "machine",
    label: "Machines",
    active: (p: string) => p.startsWith("/machines"),
  },
  {
    to: "/members",
    icon: "members",
    label: "Members",
    active: (p: string) => p.startsWith("/members"),
  },
  {
    to: "/activity",
    icon: "activity",
    label: "Activity",
    active: (p: string) => p.startsWith("/activity"),
  },
  {
    to: "/settings",
    icon: "settings",
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
          icon: "box" as const,
          active: (path: string) => path === `/plugins/${plugin.id}/${page.id}`,
        })),
      ) ?? [];
  const operatorNav = me.operator
    ? [{
      to: "/admin/plugins",
      label: "Plugins",
      icon: "plugins" as const,
      active: (path: string) => path === "/admin/plugins",
    }]
    : [];
  const nav = [...NAV, ...operatorNav, ...pluginNav];

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
  else if (p === "/admin/plugins")
    page = me.operator
      ? <AdminPlugins />
      : <ErrorNote>Operator access is required.</ErrorNote>;
  else if ((m = match("/plugins/:pluginId/:pageId", p)))
    page = <PluginPage pluginId={m.pluginId} pageId={m.pageId} />;
  else page = <p className="text-muted">Page not found.</p>;

  return (
    <Shell
      sidebar={
        <>
          <Link
            to="/"
            className="vd-hover mb-5 hidden px-2 pt-1 md:block"
            aria-label="Varde home"
          >
            <Logo className="h-8" />
          </Link>
          <label className="relative mb-4 flex items-center gap-2.5 rounded-xl border border-separator bg-background/50 p-2 transition-colors duration-150 focus-within:border-field-border-focus hover:border-field-border">
            <Avatar name={group.name} className="size-8 rounded-lg text-xs" />
            <span className="min-w-0 flex-1">
              <span className="block text-[11px] font-medium uppercase tracking-wider text-muted">
                Group
              </span>
              <span className="block truncate text-sm font-medium text-foreground">
                {group.name}
              </span>
            </span>
            <Icon name="chevrons" className="size-4 shrink-0 text-muted" />
            {/* native select over the card: keyboard and screen readers get the real control */}
            <select
              aria-label="Group"
              className="absolute inset-0 cursor-pointer opacity-0"
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
            {nav.map((n) => {
              const active = n.active(p);
              return (
                <Link
                  key={n.to}
                  to={n.to}
                  className={cx(
                    "group flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm transition-colors duration-150 ease-vd-out",
                    active
                      ? "bg-default font-medium text-foreground shadow-[inset_0_1px_0_rgb(255_255_255/0.04)]"
                      : "text-muted hover:bg-surface-secondary hover:text-foreground",
                  )}
                >
                  <Icon
                    name={n.icon}
                    className={cx(
                      "size-[18px] shrink-0 transition-colors duration-150",
                      active
                        ? "text-accent"
                        : "text-muted group-hover:text-foreground",
                    )}
                  />
                  <span className="truncate">{n.label}</span>
                </Link>
              );
            })}
          </nav>
          <div className="mt-auto flex items-center gap-2.5 rounded-xl border border-separator bg-background/50 p-2.5">
            <Avatar name={me.user.display_name} />
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-medium text-foreground">
                {me.user.display_name}
              </div>
              <div
                className="flex items-center gap-1.5 text-xs text-muted"
                title="Live updates"
              >
                <span className={connected ? "vd-pulse" : undefined}>
                  <Dot tone={connected ? "green" : "warn"} />
                </span>
                {connected ? "Live" : "Reconnecting…"}
              </div>
            </div>
            <button
              type="button"
              title="Sign out"
              aria-label="Sign out"
              className="rounded-lg p-1.5 text-muted transition-colors duration-150 hover:bg-surface-secondary hover:text-foreground"
              onClick={async () => {
                await api.POST("/v1/auth/logout");
                // A domain-wide site cookie would auto-bounce straight back
                // into a fresh session - remember the explicit sign-out.
                sessionStorage.setItem(SIGNED_OUT_KEY, "1");
                window.location.href = "/";
              }}
            >
              <Icon name="signout" className="size-[18px]" />
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
      <main className="relative min-w-0 flex-1 px-4 py-6 sm:px-6 md:px-8 md:py-8">
        <TopGlow />
        <div className="relative mx-auto max-w-5xl">{children}</div>
      </main>
    </div>
  );
}

function Centered({ children }: { children: React.ReactNode }) {
  return (
    <main className="relative flex min-h-screen items-center justify-center bg-background p-6 text-foreground">
      <TopGlow />
      <div className="relative w-full max-w-sm">{children}</div>
    </main>
  );
}
