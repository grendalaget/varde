# Frequently asked questions

## Does every player need Varde running?

Yes for the normal game connection through Varde's mesh. Each player needs the
Varde agent and its mesh process running on their computer, linked to the group.
The game connects to a stable local `127.77.x.y` address; that address is not a
public internet address. Closing the mesh removes the local forwarding path.
See [networking](architecture/networking.md).

This does not mean every player must host the server. Only one host runs the
server at a time. A configured [public HTTPS gateway](gateway.md) can expose
Auto Tournament's web interface to browsers without a local Varde client;
it is not a public gateway for every game's TCP or UDP traffic.

## Can macOS host games?

Not the current catalog games. Every entry in the
[catalog](../apps/controlplane/internal/catalog/catalog.json) allows Linux and
Windows hosts, and the scheduler rejects unsupported operating systems.
A macOS app download does not imply macOS game-hosting support. Architecture
requirements also vary by game; see the [supported games table](../README.md#supported-games-and-platforms).

## Can I run Ready Up on Windows?

Not as a CS2 server addon. Ready Up's CS2 integration requires a Linux host.
The catalog restricts CS2 game addons to Linux, and the CS2 driver rejects
server addons on other operating systems. Plain CS2 hosting is available on
Linux and Windows x86_64. See the [game notes](games/README.md#counter-strike-2).

## Does failover preserve every moment of play?

No. Failover restores replicated save snapshots, not the running game's memory
or network connections. Players may need to reconnect, and progress since the
latest usable snapshot can be lost. Save handling differs by driver; see the
[game notes](games/README.md), including Valheim's save interval and CS2's
limited persisted state.
