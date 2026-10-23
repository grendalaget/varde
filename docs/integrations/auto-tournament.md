# Auto Tournament

Varde can link a group to a self-hosted Auto Tournament (AT) instance. The
control plane uses the AT service API to import group players, create shuffle
or fixed-team tournaments, and start them. Tournament pages remain hosted by
AT.

## Link an instance

1. Configure an AT service token in the instance's `API_TOKENS` environment
   variable. Use a dedicated service token for Varde.
2. In Varde, open **Settings → Auto Tournament** and enter the AT base URL and
   service token. Linking checks the token and creates a fleet enrollment key
   for the group. The token and fleet key are encrypted at rest by Varde and
   are never returned by the API.
   When using PostgreSQL or multiple control-plane replicas, set
   `VARDE_SECRETS_KEY` to the same base64-encoded 32-byte key on every replica.
   A shared database does not get an auto-generated key; Auto Tournament
   endpoints return `503 secrets_unconfigured` until a key is configured.
3. Add each participant's SteamID64 in **Settings → Your profile**. A valid ID
   is 17 digits and starts with `7656119`; players without one cannot be
   selected for a tournament.
4. Open **Tournaments** to list existing AT tournaments or create one for the
   group. When the player checklist is left empty, Varde selects every group
   member who has a Steam ID and reports the rest as skipped.

AT match and bracket pages may require participants to sign in to the AT
instance separately.

## Control-plane network policy

The control plane does not follow redirects from AT; enter the final instance
URL directly. Private, loopback, link-local, unspecified, and shared
carrier-grade NAT addresses are allowed by default in local auth mode and
blocked in federated mode. Set `--autotournament-allow-private` or
`VARDE_AUTOTOURNAMENT_ALLOW_PRIVATE` to override this policy. Hosted control
planes should keep private-address access disabled.

## CS2 agent directive

For a linked group, Varde adds a transient `_autotournament` object to the
configuration sent to a CS2 server agent:

```json
{
  "_autotournament": {
    "fleet_url": "https://tournaments.example.com",
    "fleet_enroll_key": "<group fleet enrollment key>",
    "public_addr": "127.0.0.1:27015"
  }
}
```

`fleet_url` and `fleet_enroll_key` are always provided when a CS2 server has a
link. `public_addr` is included only when Varde has a service address with a
UDP port. This directive is not saved in the server's configuration and is not
included in server configuration API responses. The CS2 driver consumes it to
write Ready Up's `fleet.cfg`.

## Licensing

Auto Tournament is licensed under PolyForm Noncommercial. Self-hosting it for
your group is fine; Varde does not bundle or redistribute AT code.
