<div align="center">
  <img src="https://raw.githubusercontent.com/Gryt-chat/client/main/public/logo.svg" width="80" alt="Gryt logo" />
  <h1>Gryt SFU</h1>
  <p>Selective Forwarding Unit for the <a href="https://github.com/Gryt-chat/gryt">Gryt</a> voice &amp; video platform.<br />High-performance Go media server built with <a href="https://github.com/pion/webrtc">Pion WebRTC</a>.<br />Forwards audio, camera and screen-share tracks, with RTCP relay and layer-aware SVC forwarding.</p>
</div>

<br />

## Docker

```bash
docker pull ghcr.io/gryt-chat/sfu:latest
docker run -p 5005:5005 -p 3478:3478/udp --env-file .env ghcr.io/gryt-chat/sfu:latest
```

Browse tags at [ghcr.io/gryt-chat/sfu](https://github.com/Gryt-chat/sfu/pkgs/container/sfu).

## Quick start (development)

```bash
cp env.example .env
go run ./cmd/sfu
```

Starts on **http://localhost:5005**.

## Codecs

Registered in preference order: H.264, VP9, VP8, AV1. H.264 goes first because
it has the widest hardware-accelerated support across browsers and platforms,
which matters more in practice than the compression the others win on.

Video is forwarded per temporal layer rather than whole. `internal/svc` reads the
dependency descriptor and drops packets above a receiver's subscribed layer, so a
client on a poor connection gets a lower frame rate instead of a stalled track.

## Configuration

See `env.example` for all options. Key variables:

Every variable the SFU reads, and nothing it doesn't:

| Variable | Default | Description |
|----------|---------|-------------|
| `SFU_PORT` | `5005` | HTTP/WebSocket port. `PORT` is read as a fallback |
| `STUN_SERVERS` | `stun:stun.l.google.com:19302` | Comma-separated STUN servers |
| `DISABLE_STUN` | `false` | Stop discovering srflx candidates. Only safe with a direct, port-preserving path to the internet |
| `ICE_UDP_MUX_PORT` | `3478` | The one UDP port all media flows over |
| `AUDIO_TRANSPORT_PORT` | `4433` | UDP port for Gryt's own voice transport (WebTransport). Needs `ICE_ADVERTISE_IP`. `0` turns it off and clients stay on WebRTC |
| `AUDIO_UDP_PORT` | `0` (off) | UDP port for Gryt's native voice engine (plain UDP, encrypted per packet). Needs the transport above. Nothing uses it until the native apps ship |
| `ICE_ADVERTISE_IP` | — | Comma-separated IPs to advertise as host candidates, replacing the ones found on the interfaces |
| `MAX_PEERS` | `200` | How many peers may be connected at once |
| `SFU_PING_INTERVAL` | `30` | Seconds between WebSocket pings. `0` switches pinging and the read deadline off |
| `SFU_PONG_TIMEOUT` | `90` | Seconds a peer may say nothing before the SFU hangs up. Raised to two ping intervals if set lower |
| `SFU_CALL_ALONE_TIMEOUT` | `120` | Seconds one person may be the only one in a call before it ends. `0` leaves it up. Calls only — a voice channel is never ended for being quiet. Sent to clients in `room_joined`, and restarted by a `still_here` message |
| `SFU_MAX_INGEST_KBPS` | — | The most each peer should send this SFU, in kbps. Sent to clients in `room_joined`, and the engine keeps its video under it. Advisory: nothing is dropped for going over |
| `DEBUG` | `true` | Room, connection and signaling logging. Defaults on when unset |
| `VERBOSE_LOG` | `false` | RTP forwarding detail |

## Documentation

Full docs at **[docs.gryt.chat/docs/sfu](https://docs.gryt.chat/docs/host/sfu)**:

- [SFU Overview](https://docs.gryt.chat/docs/host/sfu) — architecture, track management, connection states
- [Voice Debugging](https://docs.gryt.chat/docs/host/voice-debugging) — troubleshooting audio issues
- [Deployment](https://docs.gryt.chat/docs/host) — Docker Compose, Kubernetes

## Issues

Please report bugs and request features in the [main Gryt repository](https://github.com/Gryt-chat/gryt/issues).

## Sponsors

What sponsoring pays for, the tiers, and everyone who has sponsored:
[gryt.chat/sponsors](https://gryt.chat/sponsors). To sponsor:
[GitHub Sponsors](https://github.com/sponsors/Gryt-chat).

The list itself lives in the [Gryt README](https://github.com/Gryt-chat/gryt#sponsors),
in one place rather than ten, so it cannot fall out of step across repositories.

## License

[AGPL-3.0](https://github.com/Gryt-chat/gryt/blob/main/LICENSE) — Part of [Gryt](https://github.com/Gryt-chat/gryt)
