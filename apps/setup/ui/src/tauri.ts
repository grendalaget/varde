// Typed bridge to the setup process (withGlobalTauri injects __TAURI__).
// In a plain browser — vite dev/preview — a mock stands in so the flow can
// be exercised end to end: ?mode=install|uninstall and &fail=1 to simulate
// an engine error mid-install.

export interface UiState {
  mode: "install" | "uninstall";
  version: string;
  existing_version: string | null;
  instdir: string;
  autostart: boolean;
  controlplane: boolean;
  cp_url: string | null;
  will_link: boolean;
  already_linked: boolean;
  /** Windows personalization accent (DWM), #rrggbb */
  system_accent: string | null;
}

export interface ProgressEvent {
  index: number;
  total: number;
  label: string;
}

export interface ConfirmEvent {
  url: string;
}

export interface ErrorEvent {
  message: string;
  detail?: string;
}

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

const INSTALL_LABELS = [
  "webview2",
  "migrate",
  "drain",
  "files",
  "datadirs",
  "serverurl",
  "registry",
  "services",
  "shortcuts",
  "launch",
];

function mock(): TauriGlobal {
  const q = new URLSearchParams(window.location.search);
  const mode = (q.get("mode") ?? "install") as "install" | "uninstall";
  const fail = q.get("fail") === "1";
  const http = q.get("http") === "1";
  const upgrade = q.get("upgrade") === "1";
  const listeners = new Map<string, Set<Handler<unknown>>>();
  const emit = <T>(event: string, payload: T) => {
    (window as unknown as { __mocklog?: string[] }).__mocklog = [
      ...((window as unknown as { __mocklog?: string[] }).__mocklog ?? []),
      `emit ${event} ${JSON.stringify(payload)}`,
    ];
    listeners.get(event)?.forEach((h) => h({ payload }));
  };
  const drive = (steps: string[]) => {
    let i = 0;
    const tick = () => {
      if (http && i === 5) {
        emit("setup-confirm", { url: "http://nas.lan:8080" });
        i++;
        setTimeout(tick, 400);
        return;
      }
      emit("setup-progress", {
        index: i + 1,
        total: steps.length,
        label: steps[i],
      });
      i++;
      if (fail && i === 4) {
        setTimeout(
          () =>
            emit("setup-error", {
              message: "Couldn't write Varde's files (simulated)",
              detail: "extract payload: access denied (os error 5)",
            }),
          500,
        );
        return;
      }
      if (i < steps.length) setTimeout(tick, 550);
      else setTimeout(() => emit("setup-done", {}), 500);
    };
    setTimeout(tick, 300);
  };
  return {
    core: {
      invoke: <T>(cmd: string, args?: Record<string, unknown>): Promise<T> => {
        (window as unknown as { __mocklog?: string[] }).__mocklog = [
          ...((window as unknown as { __mocklog?: string[] }).__mocklog ?? []),
          `invoke ${cmd} ${JSON.stringify(args)}`,
        ];
        switch (cmd) {
          case "get_state":
            return Promise.resolve({
              mode,
              version: "0.2.0",
              existing_version: upgrade ? "0.1.9" : null,
              instdir: "C:\\Program Files\\Varde",
              autostart: true,
              controlplane: false,
              cp_url: http ? "http://nas.lan:8080" : null,
              will_link: true,
              already_linked: false,
            } as T);
          case "start_install":
            drive(INSTALL_LABELS);
            return Promise.resolve(undefined as T);
          case "start_uninstall":
            drive(["drain", "registry", "files", "data"]);
            return Promise.resolve(undefined as T);
          case "answer_http":
          case "exit_app":
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
