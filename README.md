# codex-remote

`codex-remote` turns a machine with the `codex` CLI installed into a remotely accessible Codex host.

It is optimized for the simple case first:

1. install the tool
2. run one command
3. copy the printed block into another agent

No daemon or system service is required for the default flow.

## What it does

- starts `codex app-server` for you
- or serves a simple authenticated HTTP API over `codex exec` / `codex exec resume`
- optionally exposes it through a Cloudflare Quick Tunnel
- prints a ready-to-share handoff block with:
  - public WebSocket URL or public HTTP base URL
  - bearer token
  - minimal client snippet
- supervises the local `codex app-server` while running
- offers optional macOS `launchd` daemon commands later if you want them

## Requirements

- macOS
- `codex` installed and already logged in
- `curl` and `tar`

## One-line install

```bash
curl -fsSL https://raw.githubusercontent.com/davidmilleronline85-eng/codex-remote/main/scripts/install.sh | bash
```

What that does:

- installs the `codex-remote` binary into `~/.local/bin`
- installs `cloudflared` too, directly if needed
- leaves your existing `codex` install alone

If the repo has no release yet, the installer falls back to `go install` when Go is available.

If `codex` is missing, the script warns you and stops after installing `codex-remote`.

## One-line start

```bash
codex-remote start
```

That command:

- creates local state if needed
- starts `codex app-server`
- creates a temporary public Cloudflare Quick Tunnel
- waits until public DNS propagation and `/readyz` are both ready
- prints a copy-paste handoff block for remote agents
- keeps running in the foreground

Stop it with `Ctrl-C`.

## One-line HTTP start

```bash
codex-remote serve-http
```

That command:

- creates local state if needed
- starts a local authenticated HTTP API
- maps `POST /v1/threads` to `codex exec`
- maps `POST /v1/threads/{thread_id}/turns` to `codex exec resume`
- creates a temporary public Cloudflare Quick Tunnel
- waits until public DNS propagation and `/readyz` are both ready
- prints a copy-paste HTTP handoff block for remote agents
- keeps running in the foreground

Stop it with `Ctrl-C`.

## What you’ll see

`codex-remote start` prints a block like:

```text
============================================================
Public Codex Remote Ready
Copy this block into another agent:

BEGIN_AGENT_HANDOFF
You can interact with a remote Codex app-server over WebSocket.
The original `codex-remote start` process must stay running while you use this endpoint.
CODEX_REMOTE_URL=wss://something.trycloudflare.com
CODEX_REMOTE_TOKEN=...
AUTH_HEADER=Authorization: Bearer <CODEX_REMOTE_TOKEN>
PROTOCOL=codex-app-server-websocket
FIRST_REQUEST=initialize
...
END_AGENT_HANDOFF
...
============================================================
```

That block is the handoff. Another agent can either use the env vars directly or run the included Python snippet as-is.

`codex-remote serve-http` prints a similar `BEGIN_HTTP_AGENT_HANDOFF ... END_HTTP_AGENT_HANDOFF` block with:

- `CODEX_REMOTE_HTTP_URL`
- `CODEX_REMOTE_TOKEN`
- `POST /v1/threads`
- `POST /v1/threads/{thread_id}/turns`

## Minimal usage

Install:

```bash
curl -fsSL https://raw.githubusercontent.com/davidmilleronline85-eng/codex-remote/main/scripts/install.sh | bash
```

Start and share:

```bash
codex-remote start
```

Or use the plain HTTP wrapper:

```bash
codex-remote serve-http
```

Local-only start without a public tunnel:

```bash
codex-remote start --public=false
```

Show the current token again:

```bash
codex-remote token
```

Show health/status:

```bash
codex-remote status
```

## Python example for remote agents

```python
import asyncio, json, os, websockets

async def main():
    async with websockets.connect(
        os.environ["CODEX_REMOTE_URL"],
        additional_headers={
            "Authorization": f"Bearer {os.environ['CODEX_REMOTE_TOKEN']}",
        },
    ) as ws:
        await ws.send(json.dumps({
            "id": 1,
            "method": "initialize",
            "params": {
                "clientInfo": {"name": "remote-agent", "version": "0.1.0"},
                "capabilities": {"experimentalApi": True},
            },
        }))
        print(await ws.recv())

asyncio.run(main())
```

## HTTP example for remote agents

Create a thread:

```bash
curl -sS "$CODEX_REMOTE_HTTP_URL/v1/threads" \
  -H "Authorization: Bearer $CODEX_REMOTE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"Reply with exactly: OK","cwd":"/path/to/repo"}'
```

Resume a thread:

```bash
curl -sS "$CODEX_REMOTE_HTTP_URL/v1/threads/<thread_id>/turns" \
  -H "Authorization: Bearer $CODEX_REMOTE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"Continue from there"}'
```

## Commands

Foreground flow:

```bash
codex-remote start
codex-remote start --public=false
codex-remote serve-http
codex-remote serve-http --public=false
codex-remote status
codex-remote token
codex-remote expose quick
```

Optional daemon flow on macOS:

```bash
codex-remote daemon install
codex-remote daemon start
codex-remote daemon stop
codex-remote daemon restart
codex-remote daemon uninstall --purge
```

## Uninstall

Simple uninstall:

```bash
curl -fsSL https://raw.githubusercontent.com/davidmilleronline85-eng/codex-remote/main/scripts/uninstall.sh | bash
```

That removes:

- the `codex-remote` binary
- the bundled `cloudflared` binary if it was installed into `~/.local/bin` and you opt in
- generated local state
- the optional launchd daemon, if installed

By default it does **not** uninstall `cloudflared`, because you may use it for other things.

If you want that too:

```bash
REMOVE_CLOUDFLARED=1 curl -fsSL https://raw.githubusercontent.com/davidmilleronline85-eng/codex-remote/main/scripts/uninstall.sh | bash
```

## Self-healing

While `codex-remote start` is running, it supervises `codex app-server`.

If `codex app-server`:

- crashes
- exits
- stops answering `readyz`

`codex-remote` restarts it.

If you later choose the daemon flow, `launchd` also keeps `codex-remote` itself alive.

## Development

Build:

```bash
make build
```

Test:

```bash
make test
```

Format:

```bash
make fmt
```

## Notes

- Cloudflare Quick Tunnels are great for getting started, demos, and ad hoc remote access.
- Quick Tunnels are ephemeral; if you want a stable hostname later, add a named tunnel workflow.
- The bearer token from `codex-remote token` is sensitive. Treat it like a password.
