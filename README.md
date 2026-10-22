# pve-vm-handoff

A small web UI for handing shared hardware between Proxmox VMs. The typical case is one passthrough GPU used by two VMs: press **Start** on one, and the tool gracefully shuts the other down, waits until it's really off, and starts yours. When you're done (shut down from the UI or from inside the guest), the group's default VM starts again by itself.

Everything about that behaviour lives in one YAML file, so it works for any number of hosts, VMs and groups.

- **Exclusive groups**: members of a group never run at the same time. A VM can be in several groups (one per GPU, a shared disk, a USB controller…) and conflicts with all of their members.
- **Automatic return to a default VM**: after a handoff, always, or never, with a configurable idle delay. It can be paused from the UI.
- **Safe power handling**: graceful shutdown with a timeout and an optional hard stop afterwards. Start failures (like "PCI device already in use") are reported. Nothing happens while a host is unreachable.
- **Per-VM controls**: which buttons show up (start, shutdown, force stop), who may use them, and whether others may shut a VM down.
- **Notifications** via webhooks: JSON, or plain text for ntfy and similar.
- A single static binary of about 10 MB, with a multi-arch Docker image on GHCR.

There is **no built-in login**. Run it behind something that handles authentication: NetBird, Tailscale, Authelia, oauth2-proxy, Cloudflare Access, or just a private network.

## How it works

```
Start "Friend's VM"  ─►  shut down other running members (graceful, timeout, optional force stop)
                     ─►  wait until they are stopped
                     ─►  start "Friend's VM", wait for the task to succeed

every poll_interval  ─►  read status of all guests
                     ─►  group idle for return_delay (and the rule allows it)?  start the default VM
```

`return_to_default` decides when the default comes back:

| Mode | Behaviour |
| --- | --- |
| `after_handoff` (default) | Only when another member was the last one running. If you shut the default down yourself, it stays off. |
| `always` | Whenever no member has been running for `return_delay`. |
| `never` | No automatic start. The tool only enforces mutual exclusion. |

## Proxmox API token

Create a dedicated user and token with only the rights it needs, on the VMs it should control. Run this on a Proxmox node:

```bash
pveum role add VMHandoff --privs "VM.Audit VM.PowerMgmt"
pveum user add handoff@pve
pveum user token add handoff@pve handoff --privsep 0   # prints the secret once
pveum acl modify /vms/101 --users handoff@pve --roles VMHandoff
pveum acl modify /vms/102 --users handoff@pve --roles VMHandoff
```

Use `token_id: "handoff@pve!handoff"` and the printed secret in the config. If you prefer privilege separation (`--privsep 1`), grant the same ACLs to the token with `--tokens 'handoff@pve!handoff'`.

## Deploy with Portainer

1. **Stacks → Add stack → Web editor** and paste [`docker-compose.yml`](docker-compose.yml).
2. Edit the inline `CONFIG_YAML` with your host, VMIDs and names.
3. Under **Environment variables**, add `PVE_TOKEN_SECRET` (and `HANDOFF_PORT` if 8080 is taken).
4. Deploy, then put the port behind your auth proxy.

You can mount a file instead of using inline config: remove `CONFIG_YAML` and mount your config at `/config/config.yaml`. If `CONFIG_YAML` is set, it wins.

> The GHCR package has to be public for anonymous pulls: GitHub → Packages → pve-vm-handoff → Package settings → Change visibility.

### Docker CLI

```bash
docker run -d --name pve-vm-handoff -p 8080:8080 \
  -e PVE_TOKEN_SECRET=... \
  -v ./config.yaml:/config/config.yaml:ro -v handoff-data:/data \
  ghcr.io/yniverz/pve-vm-handoff:latest
```

To check a config without starting the server, run `pve-vm-handoff -check -config config.yaml`.

## Configuration

[`config.example.yaml`](config.example.yaml) documents every option. In short:

| Section | Key | Default | Meaning |
| --- | --- | --- | --- |
| `server` | `listen` | `:8080` | Listen address |
| | `title` | `VM Handoff` | Page title |
| | `poll_interval` | `5s` | How often guest status is read |
| | `user_header` | none | Header with the signed-in user, set by your auth proxy |
| | `state_file` | none | Keeps pause flags and last group occupant across restarts |
| `hosts.<id>` | `url`, `token_id`, `token_secret` | | Proxmox API access. For clusters, one node is enough. |
| | `verify_tls` | `true` | `false` for self-signed certificates |
| `defaults` | any VM key | | Applied to every VM before its own settings |
| `vms.<id>` | `host`, `vmid` | | Which guest. `node` and `type` are discovered automatically. |
| | `name`, `description` | | Shown in the UI |
| | `actions` | `[start, shutdown]` | Buttons offered: `start`, `shutdown`, `stop` |
| | `allowed_users` | everyone | Who may control it (needs `user_header`) |
| | `shutdown_timeout` | `3m` | Graceful shutdown limit |
| | `force_stop_after_timeout` | `false` | Hard stop when the graceful shutdown hangs |
| | `start_timeout` | `2m` | Start limit |
| | `displaceable` | `true` | Whether starting another member may shut this VM down |
| `groups.<id>` | `members` | | At least two VMs that must not run together |
| | `default` | none | VM to bring back |
| | `return_to_default` | `after_handoff` | `after_handoff`, `always` or `never` |
| | `return_delay` | `60s` | How long the group must be idle first |
| | `allowed_users` | everyone | Who may pause the automatic return |
| `notifications[]` | `url`, `method`, `format`, `headers`, `events` | `POST`, `json` | Webhooks |

Durations accept `90s`, `3m`, `1h` or plain seconds. Any value can reference environment variables as `${NAME}` or `${NAME:-fallback}`.

### User identity

When `server.user_header` is set, the UI shows who is signed in, the activity log records who did what, and `allowed_users` lists apply (case-insensitive). Use whatever header your proxy forwards, for example `X-Forwarded-Email`, `Remote-User` or `Tailscale-User-Login`. Make sure clients can't reach the container directly, or they could send that header themselves.

## API

The UI uses a small JSON API, which also works for scripts and Home Assistant:

| Method | Path | |
| --- | --- | --- |
| `GET` | `/api/state` | Groups, VMs, status, allowed actions, activity log |
| `POST` | `/api/vms/{id}/{start\|shutdown\|stop}` | Runs in the background. Returns `202`, or `409` if busy. |
| `POST` | `/api/groups/{id}/pause` | Body `{"paused": true}` |
| `GET` | `/healthz` | Liveness |

POST requests must send `X-Requested-With: pve-vm-handoff` as a basic CSRF guard.

## Development

```bash
go test ./...
CONFIG_FILE=config.yaml go run ./cmd/pve-vm-handoff
```

## License

[AGPL-3.0](LICENSE)
