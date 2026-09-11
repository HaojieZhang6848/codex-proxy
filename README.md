# codex-proxy

A small, single purpose forwarder for the Codex **Responses API**.

It sits in front of one or more upstream endpoints and rewrites exactly two
things on the way through:

1. the **model name** in the request body, and
2. the **API key** in the request header.

Everything else - paths, query strings, headers, request bytes and the streaming
response - is forwarded untouched.

## Concepts

| Concept | Configuration | Meaning |
| --- | --- | --- |
| Upstream | `upstreams[]` (`name`, `base_url`, `api_key`) | A remote Responses API endpoint this proxy can talk to. |
| Offered model | `models.<name>` (`key`) | A model name and client key that this proxy accepts. |
| Mapping | `models.<name>.upstream`, `models.<name>.upstream_model` | Which upstream to use and which model name to request there. |

```yaml
upstreams:
  - name: openai
    base_url: "https://api.openai.com/v1"
    api_key: "${OPENAI_API_KEY}"

models:
  gpt-5-codex:                 # the model name your clients ask for
    key: "${CODEX_PROXY_KEY}"  # the key your clients must send
    upstream: openai           # the upstream above
    upstream_model: gpt-5-codex # the model name sent upstream
```

A client that requests `gpt-5-codex` with `Authorization: Bearer $CODEX_PROXY_KEY`
is forwarded to `https://api.openai.com/v1/responses` with `model` replaced and
the upstream key in `Authorization`.

Requests are matched by model name first and the key is then verified against
that model. Requests without a model field (for example
`DELETE /v1/responses/{id}`) are matched by key alone; if the key maps to
several different upstream models, the proxy asks for the model name instead of
guessing.

## Quick start

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml                       # add upstreams, models and keys
export OPENAI_API_KEY=sk-...
export CODEX_PROXY_KEY=local-secret

go run ./cmd/codex-proxy validate -config config.yaml
go run ./cmd/codex-proxy run -config config.yaml
```

Point a client at it:

```bash
curl -N http://127.0.0.1:8080/v1/responses \
  -H 'Authorization: Bearer local-secret' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5-codex","input":"hello","stream":true}'
```

Codex client configuration (`~/.codex/config.toml`):

```toml
model = "gpt-5-codex"
model_provider = "codex-proxy"

[model_providers.codex-proxy]
name = "codex-proxy"
base_url = "http://127.0.0.1:8080/v1"
env_key = "CODEX_PROXY_KEY"
wire_api = "responses"
```

## Configuration

`config.yaml` is looked up in this order: `-config`, `$CODEX_PROXY_CONFIG`,
`./config.yaml`, then the platform default (`/etc/codex-proxy/config.yaml`,
`/usr/local/etc/codex-proxy/config.yaml` on macOS,
`%ProgramData%\codex-proxy\config.yaml` on Windows).

`${VAR}` and `$VAR` are expanded from the environment before parsing, so keys
can be kept out of the file.

| Key | Default | Description |
| --- | --- | --- |
| `server.listen` | `0.0.0.0:8080` | Listener, IPv4 only. |
| `server.read_header_timeout` | `30s` | Header read timeout. |
| `server.idle_timeout` | `120s` | Keep-alive idle timeout. |
| `server.shutdown_timeout` | `15s` | Graceful shutdown budget on SIGTERM. |
| `server.expose_models_endpoint` | `true` | Answer `GET /v1/models` locally. |
| `server.max_body_bytes` | `268435456` | Request body limit. |
| `proxy.url` | empty | Empty: use the system proxy. Otherwise always use this proxy (`http`, `https`, `socks5`, `socks5h`). |
| `upstreams[].name` | - | Identifier referenced by the mapping. |
| `upstreams[].base_url` | - | `http`/`https` URL, used as a path prefix. |
| `upstreams[].api_key` | - | Credential sent upstream. |
| `upstreams[].auth_header` | `Authorization` | Header carrying the key. |
| `upstreams[].auth_scheme` | `Bearer` | Scheme prefix; empty sends the raw key (Azure `api-key`). |
| `upstreams[].headers` | empty | Extra headers for this upstream; they override the incoming request's headers. |
| `upstreams[].connect_timeout` | `15s` | TCP/TLS connect timeout. |
| `upstreams[].response_header_timeout` | `120s` | Time to first byte from the upstream. |
| `models.<name>.key` | - | Key the client must present for this model. |
| `models.<name>.upstream` | - | Upstream name; optional when only one upstream exists. |
| `models.<name>.upstream_model` | the model name | Model name sent upstream. |
| `log.level` | `info` | `debug`, `info`, `warn` or `error`. |
| `log.format` | `text` | `text` or `json`. |
| `log.file` | empty | Empty logs to stdout (journald on Linux). |

Credentials are never logged: only model names, upstream names, paths, status
codes and timings.

### Paths

The `base_url` path is a prefix and the incoming path is appended. A repeated
leading `/v1` segment is collapsed once, so both spellings work:

```
base_url: "https://api.openai.com"     + /v1/responses -> /v1/responses
base_url: "https://api.openai.com/v1"  + /v1/responses -> /v1/responses
base_url: "https://host/openai/v1"     + /v1/responses -> /openai/v1/responses
```

### Networking

* **IPv4 only.** Every outbound connection is dialled as `tcp4`, so hosts with
  both A and AAAA records are never contacted over IPv6. The listener is IPv4
  too. With `socks5h` the name is resolved by the proxy, so IPv4 there depends
  on the proxy itself.
* **Proxy.** With `proxy.url` set, that proxy is always used. With an empty
  `proxy.url`, the standard `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`
  environment variables apply. Under systemd, put them in
  `/etc/codex-proxy/env` (loaded via `EnvironmentFile`), because a service does
  not inherit your shell environment.
* Responses are streamed with a flush after every chunk, so server sent events
  arrive as they are produced. `client.Do` uses no overall timeout; the request
  context is cancelled when the client disconnects.
* A body with `Content-Encoding` other than `identity` is rejected with `415`,
  because the model name inside it could not be rewritten byte for byte.

### Endpoints served locally

* `GET /healthz` - `ok`, for probes and installers.
* `GET /v1/models` - the configured model names (only when
  `server.expose_models_endpoint` is true); still requires a valid key.

## Install

### Linux (systemd)

```bash
tar -xzf codex-proxy_<version>_linux_amd64.tar.gz
cd codex-proxy_<version>_linux_amd64
sudo ./install.sh
sudo $EDITOR /etc/codex-proxy/config.yaml
sudo systemctl restart codex-proxy
```

The installer creates the `codex-proxy` system user, installs the binary to
`/usr/local/bin/codex-proxy`, the configuration to `/etc/codex-proxy/config.yaml`
and the unit to `/etc/systemd/system/codex-proxy.service`, then enables and
starts the service. `journalctl -u codex-proxy -f` shows the logs.

Override `BIN_DIR`, `CONFIG_DIR`, `STATE_DIR`, `LOG_DIR` or `SERVICE_USER` with
environment variables. `sudo ./install.sh --uninstall` removes the service and
keeps the configuration; add `--purge` to delete it as well.

### Windows (service)

```powershell
Expand-Archive codex-proxy_<version>_windows_amd64.zip -DestinationPath .
cd codex-proxy_<version>_windows_amd64
.\install.ps1
notepad "$env:ProgramFiles\codex-proxy\config.yaml"
Restart-Service codex-proxy
```

Run it from an elevated PowerShell. The installer copies `codex-proxy.exe` and
`config.yaml` into `%ProgramFiles%\codex-proxy`, registers the `codex-proxy`
service (`"<InstallDir>\codex-proxy.exe" run -config "<InstallDir>\config.yaml"`,
automatic start, restart on failure) and starts it. Logs go to the Windows Event
Log (`Get-EventLog -LogName Application -Source codex-proxy -Newest 50`) unless
`log.file` is set in the configuration.

`-InstallDir`, `-ConfigPath`, `-NoStart`, `-Uninstall` and `-Purge` are
supported.

## Build

```bash
make build     # compile for the host
make vet       # static checks
make test      # unit and end to end tests
make release   # ./build.sh
```

`./build.sh` cross compiles both targets with `CGO_ENABLED=0`, writes
`codex-proxy_<version>_<os>_<arch>.tar.gz` (Linux) and `.zip` (Windows) plus a
`SHA256SUMS` file into `dist/`, and embeds the version, commit and build time.
`TARGETS`, `VERSION`, `DIST_DIR` and `SKIP_TESTS=1` control the script.
