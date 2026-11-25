import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import type { Server } from "./src/api/client";
import { ServerList } from "./src/components/ServerList";
import { PlatformProvider, webPlatform } from "./src/lib/platform";
import { Surface } from "@varde/ui/ui";

const server = (over: Partial<Server> = {}): Server =>
  ({
    id: "server-1",
    group_id: "group-1",
    name: "Campfire Sandbox",
    game_id: "testgame",
    observed_state: "running",
    desired_state: "running",
    summary: {
      address: "127.77.238.6:7777",
      hosting_on: { node_id: "pc", name: "Gaming PC" },
      latest_safe_save: {
        snapshot_id: "save",
        created_at: Date.now() - 8000,
        replicas: [
          { node_id: "pc", name: "Gaming PC", online: true },
          { node_id: "mini", name: "Living Room Mini", online: true },
        ],
      },
    },
    ...over,
  }) as Server;

function render(s: Server, crossplay = false, desktop = false) {
  return renderToStaticMarkup(
    <PlatformProvider
      platform={
        desktop
          ? {
              ...webPlatform,
              kind: "desktop",
              thisGroupId: "group-1",
              joinCs2Server: async () => {},
            }
          : webPlatform
      }
    >
      <ServerList entries={[{ server: s, gameName: "Test Game", crossplay }]} />
    </PlatformProvider>,
  );
}

test("table and phone cards keep names, state, host, address copy and safe-save replicas", () => {
  const html = render(server());
  for (const column of [
    "Server",
    "Status",
    "Host",
    "Address",
    "Latest safe save",
    "Backups",
  ])
    expect(html).toContain(`>${column}</th>`);
  expect(html).toContain('scope="row"');
  expect(html).toContain('<tr class="vd-hairline-row">');
  expect(html).toContain('<header class="vd-hairline-end">');
  expect(html).toContain('href="/servers/server-1"');
  expect(html).toContain('aria-label="Copy 127.77.238.6:7777"');
  expect(html).toContain("Living Room Mini");
  expect(html).toContain("Gaming PC is hosting this server");
  expect(html).toContain('class="server-cards"');
});

test("stopped servers and an empty list do not invent hosts, addresses or saves", () => {
  const html = render(server({ observed_state: "stopped", summary: {} }));
  expect(html).toContain("Not running");
  expect(html).toContain("None yet");
  expect(html).not.toContain("hosting this server");
  expect(html).not.toContain('aria-label="Copy');
  expect(renderToStaticMarkup(<ServerList entries={[]} />)).not.toContain(
    'scope="row"',
  );
});

test("failed and unsafe/offline saves retain attention and safety warnings", () => {
  const html = render(
    server({
      observed_state: "failed",
      summary: {
        latest_save: { snapshot_id: "s", created_at: 1, replicas: [] },
      } as Server["summary"],
    }),
  );
  expect(html).toContain("Needs attention");
  expect(html).toContain("No machine with the latest save is online");
  expect(html).toContain("bg-warning-soft");
});

test("join codes and public URLs keep their copy and external-link behavior", () => {
  expect(render(server({ summary: { join_code: "ABC123" } }), true)).toContain(
    'aria-label="Copy ABC123"',
  );
  expect(render(server({ summary: {} }), true)).toContain("Waiting for code…");
  const html = render(server({ public_url: "https://game.example/join" }));
  expect(html).toContain('href="https://game.example/join"');
  expect(html).toContain('target="_blank"');
});

test("nested surfaces have a single frame", () => {
  const html = renderToStaticMarkup(
    <Surface>
      <Surface>Content</Surface>
    </Surface>,
  );
  expect(html.match(/vd-surface/g)).toHaveLength(1);
});

test("only a running desktop CS2 server with an address has Join server", () => {
  const cs2 = server({
    game_id: "cs2",
    summary: { address: "127.77.238.6:27015" },
  });
  const html = render(cs2, false, true);
  expect(html.match(/Join server/g)).toHaveLength(2);
  expect(html).toContain('aria-label="Copy 127.77.238.6:27015"');
  expect(render(cs2)).not.toContain("Join server");
  expect(
    render({ ...cs2, group_id: "another-group" }, false, true),
  ).not.toContain("Join server");
  expect(render(server(), false, true)).not.toContain("Join server");
  expect(
    render({ ...cs2, observed_state: "starting" }, false, true),
  ).not.toContain("Join server");
  expect(
    render({ ...cs2, desired_state: "stopped" }, false, true),
  ).not.toContain("Join server");
  expect(render({ ...cs2, summary: {} }, false, true)).not.toContain(
    "Join server",
  );
});
