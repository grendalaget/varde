# Control-plane plugins

Varde plugins are independently hosted HTTP services. The control plane reads a JSON manifest at startup, renders its declared settings and pages with native Varde UI, and calls the service over HTTP. A plugin does not run JavaScript in a Varde user session. Plugin rows for manifests that are not loaded remain in the database and are ignored.

Steam IDs shown to plugins are verified through Steam sign-in; users cannot set them by typing an ID.

## Running a plugin

Provide a manifest with `--plugin /path/to/plugin.json` (repeatable) or a comma-separated `VARDE_PLUGINS` environment variable. Invalid manifests stop startup and identify the file. Plugin IDs are lowercase ASCII letters/digits/hyphens, start with a letter, and are 2–32 characters.

```json
{
  "id": "example-plugin",
  "name": "Example",
  "version": "1.0.0",
  "base_url": "http://127.0.0.1:7300",
  "secret_env": "EXAMPLE_PLUGIN_SECRET",
  "service_token": "plugin-example",
  "settings": [
    {"name": "api_key", "type": "secret", "required": true},
    {"name": "region", "type": "select", "options": ["eu", "us"]}
  ],
  "pages": [{
    "id": "dashboard",
    "title": "Dashboard",
    "list": {"endpoint": "/varde/tournaments"},
    "actions": [{
      "id": "create",
      "title": "Create",
      "endpoint": "/varde/tournaments",
      "fields": [{"name": "title", "type": "string", "required": true}]
    }]
  }]
}
```

Exactly one of `secret_env` and `secret_file` is required; the resolved value must contain at least 32 bytes. Trailing `\r` and `\n` bytes are ignored when reading `secret_file`; plugins loading that file must do the same. `base_url` must be HTTP(S), without a query or fragment; trailing slashes are removed. List and action endpoints must begin with `/`, contain no `..`, and be relative paths. Fields follow the game-config shape (`string`, `int`, `bool`, `select`, `secret`); `multiselect` requires options and accepts selected strings, while `member_multiselect` accepts group user IDs and may declare `"requires":"steam_id"`. Mark service-generated settings with `"hidden":true`; users cannot submit them.

The control plane sends requests to the configured base URL, so operators should bind plugin services only to trusted networks and configure firewall access accordingly. The plugin's secret is used only for request signing; Varde does not pass it in the envelope.

## Request authentication and envelope

Every call is a POST with a JSON envelope:

```json
{
  "group_id": "group-id",
  "user": {"id": "user-id", "display_name": "Member", "role": "member"},
  "settings": {"api_key": "decrypted value", "internal_state": "hidden values included"}
}
```

Action calls add `"values"`; list, settings, and disable calls omit it. `X-Varde-Op` is `list`, `action:<action-id>`, `settings`, or `disable`. Plugin settings and disable hooks use `/varde/v1/settings` and `/varde/v1/disable`.

`Authorization` is a short-lived HS256 JWT signed with the manifest secret. Claims are `iss:"varde-control-plane"`, `aud:<plugin id>`, `sub:<user id>`, `grp:<group id>`, `role:<member role>`, `iat`, `exp` (60 seconds after `iat`), and a random `jti`. Verify the algorithm and claims before processing:

```js
const { payload } = await jwtVerify(token, new TextEncoder().encode(secret), {
  algorithms: ["HS256"],
  issuer: "varde-control-plane",
  audience: "example-plugin",
});
```

## Replies and errors

Every 2xx reply must be JSON. A list endpoint returns `{"items":[{"title":"…","subtitle":"…","status":"…","href":"https://…"}]}`; `href`, when present, must be HTTP(S). Action replies use `{"message":"…","href":"https://…","notes":["…"]}`; `href` is optional and must be HTTP(S). A settings hook may return `{"store":{"hidden_setting":"value"}}`; only declared hidden settings are merged. Disable is best-effort.

A plugin 4xx reply of `{"error":"message"}` becomes a 400 validation response (message truncated to 300 characters). A 5xx, timeout, invalid JSON, oversized body, or invalid reply becomes 502 `plugin_unavailable`; the message is `<plugin name> is unavailable`. Plugin response bodies are limited to 1 MiB; calls time out after 10 seconds and redirects are not followed.

## Settings storage and service tokens

Secret settings and hidden settings are sealed separately at rest using AES-256-GCM and associated data `<group_id>/plugin:<plugin_id>/<field>`. SQLite deployments create a local key file on first startup. PostgreSQL/shared deployments must set `VARDE_SECRETS_KEY` to the same base64-encoded 32-byte key on every control-plane replica, or configure a shared `--secrets-key-file`. Without a key, plugin settings and actions return 503 `secrets_unconfigured`. Back up this key with the database; losing it makes saved plugin secrets unreadable.

Operators create service tokens with `varde-control-plane service-token create`. Available scopes include `servers`, `events`, and `steam_ids` in addition to existing scopes. If a loaded manifest declares `service_token`, the matching token can act only in groups where that plugin is enabled. Service tokens need `steam_ids` to see member Steam IDs; signed-in group members continue to see them.

Plugins are out-of-tree services and are not bundled with Varde. Review each service and its network access before enabling it.

## Server addons

A plugin may manage typed server addons through the control-plane API. Each addon declares an HTTPS archive URL, lowercase SHA-256 digest, `zip` or `tar_gz` archive type, a driver target, JSON driver options (up to 4 KiB), and up to 32 small relative files (64 KiB each and 2 MiB total). File keys use slash-separated relative paths and cannot contain traversal segments, backslashes, or colons. HTTP archive URLs are accepted only in control-plane development mode or for loopback hosts.

Listing addons requires membership in the server's group. Addon writes require a service token with the `server_addons` scope, bound to the same group, whose name matches the plugin's configured service token; the plugin must also be enabled in that group. A group owner or administrator may delete an addon. Stored specifications are encrypted with the control-plane secrets key. Disabling the plugin or deleting the server removes its addons.

Addon changes take effect at the next server start; the control plane does not restart a running server. The agent verifies and fetches each archive through its runtime provider, then passes a `PreparedAddon` to a driver that advertises the corresponding target in `addon_targets`. Untrusted addon downloads are capped at 512 MiB, archives at 20,000 entries and 1 GiB unpacked; only regular files and directories are accepted. Symlinks, hard links, devices, and unsafe paths are rejected. Drivers install archive contents and supplied files into their target-specific location. Ready Up is a motivating example: an event plugin could manage a server-side rules bundle whose files are applied by the game driver at startup.
