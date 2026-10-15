// Typed bridge to the tray process (withGlobalTauri injects __TAURI__ before
// page scripts run). In a plain browser — vite dev/preview — a small mock
// stands in so the views can be exercised without the service: pick a canned
// status with ?state=not_linked|linking|online|hosting|offline|locked|noagent
// (locked = an administrator's pending re-link).

export interface UiStatus {
  state: string;
  control_plane_url: string;
  group_name: string;
  node_name: string;
  user_code: string;
  link_url: string;
  expires_at_unix_ms: number;
  link_error: string;
  link_error_detail: string;
  hosting: boolean;
  relink: boolean;
}

export interface LinkDefaults {
  url: string;
  auto: boolean;
  relink: boolean;
  // "windows" | "macos" — a few service strings are platform-specific
  os: string;
}

// "federated-auth" payloads from the tray ("Log in with Varde" progress).
export type FedState =
  | { stage: "confirm"; email: string; name: string }
  | { stage: "waiting_browser" }
  | { stage: "exchanging" }
  | { stage: "pick_group"; groups: { id: string; name: string }[] }
  | { stage: "approving" }
  | { stage: "error"; message: string; detail?: string };

type Handler<T> = (e: { payload: T }) => void;
type Unlisten = () => void;

interface TauriGlobal {
  core: {
    invoke: <T>(cmd: string, args?: Record<string, unknown>) => Promise<T>;
  };
  event: {
    listen: <T>(event: string, handler: Handler<T>) => Promise<Unlisten>;
  };
}

declare global {
  interface Window {
    __TAURI__?: TauriGlobal;
  }
}

function linked(hosting: boolean): UiStatus {
  return {
    state: "online",
    control_plane_url: "https://cp.varde.games",
    group_name: "Grendalaget",
    node_name: "desktop-42",
    user_code: "",
    link_url: "",
    expires_at_unix_ms: 0,
    link_error: "",
    link_error_detail: "",
    hosting,
    relink: false,
  };
}

const canned: Record<string, UiStatus | null> = {
  not_linked: {
    state: "not_linked",
    control_plane_url: "https://cp.varde.games",
    group_name: "",
    node_name: "",
    user_code: "",
    link_url: "",
    expires_at_unix_ms: 0,
    link_error: "",
    link_error_detail: "",
    hosting: false,
    relink: false,
  },
  linking: {
    state: "linking",
    control_plane_url: "https://cp.varde.games",
    group_name: "",
    node_name: "",
    user_code: "WXYZ-1234",
    link_url: "https://cp.varde.games/link?code=WXYZ-1234",
    expires_at_unix_ms: Date.now() + 298_000,
    link_error: "",
    link_error_detail: "",
    hosting: false,
    relink: false,
  },
  online: linked(false),
  hosting: linked(true),
  offline: { ...linked(false), state: "offline" },
  locked: {
    ...linked(false),
    state: "linking",
    user_code: "WXYZ-1234",
    link_url: "https://cp.varde.games/link?code=WXYZ-1234",
    expires_at_unix_ms: Date.now() + 298_000,
    relink: true,
  },
  noagent: null,
};

function mock(): TauriGlobal {
  const state =
    new URLSearchParams(window.location.search).get("state") ?? "not_linked";
  // keyed by event name like the real bridge: an emit must only reach the
  // listeners for that event
  const listeners = new Map<string, Set<Handler<unknown>>>();
  const emit = <T>(event: string, payload: T) => {
    // TEMP-TEST: trace event emissions for verification
    (window as unknown as { __mocklog?: string[] }).__mocklog = [
      ...((window as unknown as { __mocklog?: string[] }).__mocklog ?? []),
      `emit ${event} ${JSON.stringify(payload)}`,
    ];
    listeners.get(event)?.forEach((h) => h({ payload }));
  };
  return {
    core: {
      invoke: <T>(cmd: string, args?: Record<string, unknown>): Promise<T> => {
        // TEMP-TEST: trace invokes for verification
        (window as unknown as { __mocklog?: string[] }).__mocklog = [
          ...((window as unknown as { __mocklog?: string[] }).__mocklog ?? []),
          `invoke ${cmd} ${JSON.stringify(args)}`,
        ];
        switch (cmd) {
          case "get_status":
            return Promise.resolve(canned[state] as T);
          case "link_defaults":
            return Promise.resolve({
              url: "https://cp.varde.games",
              auto: false,
              relink: false,
              os:
                new URLSearchParams(window.location.search).get("os") ??
                "windows",
            } as T);
          case "begin_varde_login": {
            emit("federated-auth", { stage: "waiting_browser" });
            // the browser "returned" a varde:// callback — confirm next
            setTimeout(
              () =>
                emit("federated-auth", {
                  stage: "confirm",
                  email: "arne@example.com",
                  name: "Arne",
                }),
              500,
            );
            return Promise.resolve(undefined as T);
          }
          case "confirm_varde_login": {
            emit("federated-auth", { stage: "exchanging" });
            const fedp = new URLSearchParams(window.location.search).get("fed");
            const multi = fedp === "multi";
            // TEMP-TEST: simulate a federated-auth failure with ?fed=error
            const err = fedp === "error";
            setTimeout(
              () =>
                err
                  ? emit("federated-auth", {
                      stage: "error",
                      message: "Couldn't reach varde.games (simulated)",
                      detail: "api 404 http_error: 404 page not found",
                    })
                  : multi
                    ? emit("federated-auth", {
                        stage: "pick_group",
                        groups: [
                          { id: "grp_1", name: "Grendalaget" },
                          { id: "grp_2", name: "Weekend crew" },
                        ],
                      })
                    : emit("federated-auth", { stage: "approving" }),
              400,
            );
            if (!multi && !err) {
              setTimeout(() => emit("status", canned.linking), 900);
              setTimeout(() => emit("status", linked(false)), 3500);
            }
            return Promise.resolve(undefined as T);
          }
          case "cancel_varde_login":
            emit("status", canned.not_linked);
            return Promise.resolve(undefined as T);
          case "federated_state":
            return Promise.resolve(null as T);
          case "finish_varde_login": {
            emit("federated-auth", { stage: "approving" });
            setTimeout(() => emit("status", canned.linking), 400);
            setTimeout(() => emit("status", linked(false)), 2500);
            return Promise.resolve(undefined as T);
          }
          case "start_link":
            emit("status", canned.linking);
            // pretend the owner approved a few seconds later
            setTimeout(() => emit("status", linked(false)), 4000);
            return Promise.resolve(canned.linking as T);
          case "cancel_link":
            emit("status", canned.not_linked);
            return Promise.resolve(undefined as T);
          case "open_dashboard":
          case "open_url":
          case "start_service":
          case "stop_service":
          case "close_window":
            return Promise.resolve(undefined as T);
          default:
            return Promise.reject(new Error(`mock: unknown command ${cmd}`));
        }
      },
    },
    event: {
      listen: <T>(event: string, handler: Handler<T>): Promise<Unlisten> => {
        let set = listeners.get(event);
        if (!set) {
          set = new Set();
          listeners.set(event, set);
        }
        const h = handler as Handler<unknown>;
        set.add(h);
        return Promise.resolve(() => {
          set.delete(h);
        });
      },
    },
  };
}

const tauri: TauriGlobal = window.__TAURI__ ?? mock();

export const invoke = tauri.core.invoke;
export const listen = tauri.event.listen;
