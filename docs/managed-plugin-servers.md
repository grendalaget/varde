# Plugin-managed servers

A plugin can opt into a group-owned server using this manifest block:

```json
{
  "managed_server": {
    "game": "autotournament",
    "mode_setting": "hosting_mode",
    "config": { "licence": "readyup_license" },
    "bindings": {
      "at_url": "public_url",
      "at_token": "secret:api_token"
    }
  }
}
```

`game` is a catalog ID. `mode_setting` names a visible select with exactly two
options and a default: the default means managed; the other means manual.
`config` maps non-secret game config field names to visible plugin setting names.
Required game fields without defaults must be mapped. The source setting must
have a compatible type. A licence setting without a default must be explicitly
chosen before enabling/starting the managed plugin.

`bindings` maps visible string/secret plugin settings to runtime sources:

- `public_url`: the gateway URL, requiring a gateway-published catalog game.
- `secret:<config field>`: a secret game-config field. The plugin target must also
  be secret. Varde generates 32 random bytes, persists the URL-safe value encrypted
  with the control-plane secrets key, and injects it into agent runtime config.
  It is not included in ordinary server config or deployment JSON/API responses.
- `setup_code`: the current serving execution's reported join/setup code. When
  this source is requested, binding waits until the code is known.

Bound fields are omitted from group settings responses and reject user writes in
managed mode. In manual mode they retain the normal editable/required semantics.
`GET /v1/plugins` includes `managed_server`; dashboard forms can use its binding
keys and the selected/default mode to hide and disable the corresponding fields.
Saving managed settings enables the plugin without prematurely calling its hook.
Switching to manual mode or disabling the plugin stops its managed server.

## Group API

All three routes require an owner/admin user session, not a service token:

```text
GET  /v1/groups/{groupId}/plugins/{pluginId}/managed-server
POST /v1/groups/{groupId}/plugins/{pluginId}/managed-server/start
POST /v1/groups/{groupId}/plugins/{pluginId}/managed-server/stop
```

Start has no body. First save the plugin's group settings through its existing
`PUT /v1/groups/{groupId}/plugins/{pluginId}` route. Start creates or reuses one
server per group/plugin and uses normal limits, placement and lease handling.
`group_pc_required` and `gateway_unconfigured` retain their existing meanings.
Stop does not remove the server or its saves.

Responses contain `phase` and, when available, `server_id`, `public_url`,
`setup_code`, `node_name` and `reason`. Phases are `stopped`, `preparing`, `starting`,
`ready` and `failed`. `preparing` includes runtime fetch, installation and restore;
the agent does not report a distinct download phase or byte progress. `ready`
requires a healthy current serving execution and an acknowledged settings hook.
`stopped` represents stopped desired state; child shutdown is asynchronous.

## Binding reconciliation

Only the primary reconciles bindings, using the same leadership/migration gate as
the plugin supervisor. Once the serving execution reports health, Varde persists
bound settings using the existing settings encryption, calls the existing
`/varde/v1/settings` hook, validates/persists its hidden `store` values, and records
the acknowledged binding digest. The final hook result and digest commit together.

Unchanged acknowledged settings do not call the hook again, including after a
restart or promotion. URL, secret or visible-setting changes produce a new digest.
Failures retain a sanitized reason in status and retry after 1, 2, 4, ... 64 seconds;
retry state is durable. A rejected hook produces `failed` while retries continue.

Hook delivery is at least once across a crash between HTTP success and durable
acknowledgement. Each retry uses fresh authentication and the same
`Idempotency-Key: managed-server:<server ID>:<settings digest>` header. Hooks with
side effects must honor that key or otherwise implement idempotent settings
application. There is no distributed transaction with an external plugin.

All replicas must use the same `VARDE_SECRETS_KEY`, as for other plugin settings.
