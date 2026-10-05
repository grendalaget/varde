import { useCallback, useEffect, useMemo, useState } from "react";
import { api, errorMessage, type Me, type Node } from "./api/client";
import Cairn from "./components/Cairn";
import { cx, Dot, ErrorNote } from "./components/ui";
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

const GROUP_KEY = "varde.group";

export default function App() {
  const [me, setMe] = useState<Me | null | undefined>(undefined);
  const [loadError, setLoadError] = useState<string | null>(null);
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

  useEffect(() => {
    void refreshMe();
  }, [refreshMe]);

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
          <p className="text-sm text-slate-400">Loading…</p>
        )}
      </Centered>
    );
  }

  if (me === null) return <Login onAuthed={refreshMe} />;

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
  else page = <p className="text-slate-400">Page not found.</p>;

  return (
    <div className="flex min-h-screen bg-slate-950 text-slate-100">
      <aside className="flex w-56 shrink-0 flex-col border-r border-slate-800 bg-slate-950 px-3 py-4">
        <Link to="/" className="mb-6 flex items-center gap-2 px-2">
          <Cairn />
          <span className="text-lg font-semibold tracking-tight">Varde</span>
        </Link>
        <label className="mb-4 px-2">
          <span className="text-[11px] font-medium uppercase tracking-wider text-slate-500">
            Group
          </span>
          <select
            className="mt-1 w-full rounded-md border border-slate-800 bg-slate-900 px-2 py-1.5 text-sm"
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
        <nav className="flex flex-col gap-0.5">
          {NAV.map((n) => (
            <Link
              key={n.to}
              to={n.to}
              className={cx(
                "rounded-md px-2 py-1.5 text-sm",
                n.active(p)
                  ? "bg-slate-800 font-medium text-white"
                  : "text-slate-400 hover:bg-slate-900 hover:text-slate-200",
              )}
            >
              {n.label}
            </Link>
          ))}
        </nav>
        <div className="mt-auto space-y-3 px-2 text-xs text-slate-500">
          <div className="flex items-center gap-2" title="Live updates">
            <Dot tone={connected ? "green" : "amber"} />
            {connected ? "Live" : "Reconnecting…"}
          </div>
          <div className="truncate text-slate-400">{me.user.display_name}</div>
          <button
            className="text-slate-500 hover:text-slate-300"
            onClick={async () => {
              await api.POST("/v1/auth/logout");
              window.location.href = "/";
            }}
          >
            Sign out
          </button>
        </div>
      </aside>
      <main className="min-w-0 flex-1 px-8 py-8">
        <div className="mx-auto max-w-5xl">{page}</div>
      </main>
    </div>
  );
}

function Centered({ children }: { children: React.ReactNode }) {
  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-950 p-6 text-slate-100">
      <div className="w-full max-w-sm">{children}</div>
    </main>
  );
}
