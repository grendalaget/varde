import { setApiTransport } from "@varde/dashboard/api/client";
import { setEventStream } from "@varde/dashboard/lib/events";

import { invoke } from "./tauri";

type CpResponse = {
  status: number;
  body: string;
};

type CpEvent =
  { kind: "open" } | { kind: "message"; data: string } | { kind: "error" };

export function installCpTransport(): void {
  setApiTransport(async (request) => {
    const url = new URL(request.url);
    const body =
      request.method === "GET" || request.method === "HEAD"
        ? undefined
        : await request.text();
    const response = await invoke<CpResponse>("cp_fetch", {
      request: {
        method: request.method,
        path: url.pathname + url.search,
        body,
      },
    });
    const responseBody =
      response.status === 204 ||
      response.status === 205 ||
      response.status === 304 ||
      response.body.length === 0
        ? null
        : response.body;
    return new Response(responseBody, {
      status: response.status,
      headers: { "content-type": "application/json" },
    });
  });

  setEventStream((groupId, handlers) => {
    const tauri = window.__TAURI__;
    if (!tauri) {
      handlers.onError();
      return () => {};
    }

    const channel = new tauri.core.Channel<CpEvent>();
    let disposed = false;
    let subscriptionId: number | undefined;
    const unsubscribe = (id: number) => {
      void invoke<void>("cp_events_unsubscribe", { id }).catch(() => {});
    };
    channel.onmessage = (event) => {
      switch (event.kind) {
        case "open":
          handlers.onOpen();
          break;
        case "message":
          handlers.onMessage(event.data);
          break;
        case "error":
          handlers.onError();
          break;
      }
    };
    void invoke<number>("cp_events_subscribe", {
      groupId,
      onEvent: channel,
    })
      .then((id) => {
        if (disposed) {
          unsubscribe(id);
        } else {
          subscriptionId = id;
        }
      })
      .catch(() => {
        if (!disposed) handlers.onError();
      });

    return () => {
      disposed = true;
      if (subscriptionId !== undefined) unsubscribe(subscriptionId);
    };
  });
}
