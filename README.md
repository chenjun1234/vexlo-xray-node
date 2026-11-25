# Personal VLESS over WebSocket

One authenticated VLESS user, `decryption: none`, cleartext WebSocket. Built for a platform that builds a public Dockerfile, injects environment variables, exposes port 8080, and terminates TLS on its HTTPS edge. No identity is built into the image.

Xray-core **v26.11.9** (`676eaf6`) is downloaded from the XTLS GitHub release and checked against the published SHA-256. That release still serves VLESS over WebSocket. It also prints a startup warning that WebSocket is deprecated in favor of XHTTP. This image stays on WebSocket.

## Environment

| Name | Required | Value |
| --- | --- | --- |
| `CLIENT_ID` | yes | One UUID. Canonical form, version nibble 1–8, RFC 4122 variant. |
| `WS_PATH` | yes | WebSocket path: `/` plus 16–128 characters from `A-Za-z0-9._~-`. |
| `PORT` | no | Public listen port. Default `8080`. `18080` is reserved and refused. |

Missing or malformed values exit before Xray starts. There is no sample user and no sample path.

Generate the values locally and store them only in the platform's variable UI or an untracked `.env`:

```sh
uuidgen
printf '/%s\n' "$(openssl rand -hex 16)"
```

[`.env.example`](.env.example) is intentionally empty and will not start.

The process does not print the UUID, path, generated config, share link, or environment. Xray's own access and error logs are disabled. Its process may still print its version banner, the temporary config file name, and the upstream WebSocket deprecation warning. The config file is mode `0600` under `/tmp` and is removed after Xray has loaded it. `/tmp` must be writable.

`PUBLIC_HOST` is not read. The public hostname belongs in the client, not in this container.

## Vexlo

Public project setup is: create a project from the public Git URL, choose the Dockerfile build, set the exposed port to **8080**, and set variables before deploy. The platform assigns HTTPS on 443 and terminates TLS. Put `CLIENT_ID` and `WS_PATH` in that variable store (a `.env` import is fine if the file stays out of Git). Leave `PORT` unset, or set `PORT=8080`.

Vexlo's public docs describe HTTP uptime probes and zero-downtime health checks. They do not publish the probe path, the expected status, or a switch between HTTP and TCP. This image does not add a Vexlo-specific health setting.

## Health

The public listener is `0.0.0.0:${PORT:-8080}`.

- `GET` and `HEAD` of `/` and `/healthz` return `200` when the loopback VLESS listener is accepting connections. `GET` body is `ok\n`. If that listener is down, the response is `503`.
- Any other plain HTTP request, including `GET` of `WS_PATH`, returns `404` and is not forwarded.
- A WebSocket upgrade is forwarded only when the path matches `WS_PATH` exactly. The query string is left intact so a client may use early data (`?ed=`).
- A TCP connect to `PORT` only means the front listener is up. It can succeed before VLESS is ready and does not check the UUID.
- The image `HEALTHCHECK` is an HTTP `GET /` on `127.0.0.1:$PORT`.

Use an HTTP probe of `/` when the platform sends one. Treat a TCP probe as a listener check only.

## What the proxy will reach

Xray's freedom outbound uses that version's default safety policy: traffic that arrives through VLESS is not forwarded to private, loopback, or link-local addresses. Public destinations are forwarded. There is no second user, API, stats port, or admin panel.

## Client

Inside the platform the origin hop is cleartext WebSocket with no TLS. A client outside uses the platform hostname, port 443, and TLS. Do not set a VLESS flow; Vision does not apply to WebSocket.

Fill this in locally and do not commit it:

```text
vless://<CLIENT_ID>@<public-hostname>:443?encryption=none&security=tls&type=ws&path=<percent-encoded-WS_PATH>
```

Local tests in this repo spoke cleartext WebSocket to the container port. They did not deploy to Vexlo and did not exercise the platform TLS edge.

## Run

The image runs as uid 65532.

```sh
docker build -t vexlo-xray-node .
docker run --rm -p 8080:8080 \
  -e CLIENT_ID \
  -e WS_PATH \
  vexlo-xray-node
```

Pass `CLIENT_ID` and `WS_PATH` from the shell environment. Do not bake them into the image.
