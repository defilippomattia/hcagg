# hcagg

hcagg (health check aggregator) combines the health checks of many services into one endpoint. Your load balancer calls one URL, and hcagg calls every service's own health check. It answers 200 if all of them pass and 503 if any of them fails.

Why use it:

- **All or nothing:** for applications made of many microservices, often built by different teams, that only work if all of them work. If one service fails, the load balancer takes the whole VM out of rotation.
- **Simple load balancer setup:** one monitor per VM, not one per service, each with its own path and expected status.
- **No load balancer changes:** when a team adds a service or changes a health check endpoint, you update the hcagg config. The team that manages the load balancer doesn't have to change anything.

<img src="https://github.com/user-attachments/assets/ff8f539c-281b-4327-a4a0-5c8ee6f2aa0b" alt="Without hcagg the load balancer needs one monitor per service on every VM; with hcagg it needs one per VM" width="100%">

## Configuration

hcagg is configured with a JSON file:

| Field | Required | Default | Description |
|---|---|---|---|
| `listen` | no | `:9050` | Address and port hcagg listens on. Use a specific IP, e.g. `10.0.0.5:9050`, to listen on one interface only. |
| `path` | no | `/health` | URL path that returns the combined health result. |
| `timeout` | no | `5s` | Timeout for the checks, e.g. `500ms`, `2s`. All checks run in parallel and share it, so it's also the longest a request to `path` can take. |
| `log_level` | no | `info` | `error`: only failed checks are logged, with the reason (recommended for production). `info`: also passed checks and a summary on every request.|
| `response_mode` | no | `full` | `simple` or `full`. See [Response](#response). |
| `tls_cert` | no | — | Certificate (PEM) for serving HTTPS. Put any intermediate after the server certificate. Set together with `tls_key`; if neither is set, hcagg serves plain HTTP. |
| `tls_key` | no | — | Private key (PEM) for `tls_cert`. |
| `ca_file` | no | — | Extra CA certificate(s) (PEM) to trust when calling `https://` checks, on top of the system's CAs. Use it when services have certificates from an internal CA. |
| `insecure_skip_verify` | no | `false` | Don't verify the certificates of `https://` checks. Only as a temporary workaround. |
| `checks` | yes | — | List of health checks, at least one. |
| `checks[].name` | yes | — | Name shown in the response and in the logs. |
| `checks[].url` | yes | — | Full URL of the service's health check, `http://` or `https://`. |
| `checks[].method` | yes | — | HTTP method, e.g. `GET` or `HEAD`. |
| `checks[].expected_status` | yes | — | The check passes only if the response has exactly this status code, e.g. `200`. |

hcagg refuses to start if the config is invalid: a required field is missing, a value isn't allowed, only one of `tls_cert` and `tls_key` is set, or `ca_file` can't be read.

### Response

hcagg runs all checks in parallel on every request to `path`.

- **200** if every check passed.
- **503** if any check failed (wrong status code, timeout, connection error, certificate error).

The status code is the same in both modes. The `response_mode` setting only changes the body.

#### `simple`

Plain text: `OK` if every check passed, `FAIL` if not. Nothing about the individual checks is shown; see the logs for that.

#### `full` (default)

JSON with the result of each check:

```json
{
  "healthy": false,
  "time": "2026-11-14T06:00:00Z",
  "checks": [
    { "name": "users-service", "url": "http://users.internal:8080/health", "healthy": true, "status": 200, "latency_ms": 12 },
    { "name": "payments-service", "url": "https://payments.internal/healthz", "healthy": false, "latency_ms": 3000, "error": "context deadline exceeded" }
  ]
}
```

## Running

To apply config changes without a restart, send hcagg the `HUP` signal (commands below). If the new config is valid, hcagg switches to it. If it's invalid, hcagg logs the error and keeps the old one. `listen`, `tls_cert` and `tls_key` can only change with a restart.

### As a systemd service

[hcagg.service](hcagg.service) is an example unit: it runs hcagg as an unprivileged `hcagg` user and reads `/etc/hcagg/config.json`. Change it to your liking.

```bash
curl -fL -o hcagg https://github.com/defilippomattia/hcagg/releases/latest/download/hcagg-linux-amd64
sudo useradd -r -s /usr/sbin/nologin hcagg
sudo install -m 755 hcagg /usr/local/bin/hcagg
sudo install -d -m 750 -g hcagg /etc/hcagg
sudo install -m 640 -g hcagg config.json /etc/hcagg/config.json
sudo cp hcagg.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hcagg
```

```bash
sudo systemctl reload hcagg      # reload the config
sudo systemctl restart hcagg     # restart
journalctl -u hcagg -f           # logs
```

Files in `/etc/hcagg` (including `ca_file`, `tls_cert` and `tls_key`) must be readable by the `hcagg` group, e.g. owned by `root:hcagg` with mode 640.

### As a Docker container

```bash
docker run -d --name hcagg --restart unless-stopped \
  -p 9050:9050 -v /etc/hcagg:/etc/hcagg:ro ghcr.io/defilippomattia/hcagg:1.0.0

docker kill --signal=HUP hcagg   # reload the config
docker logs -f hcagg             # logs
```

The image reads `/etc/hcagg/config.json`. Mount the folder, not the single file: many editors save by replacing the file, and a single-file mount would keep showing the container the old version. Files in it must be readable by a non-root user (e.g. mode 644). If you use `ca_file`, `tls_cert` or `tls_key`, put them in the same folder and use their `/etc/hcagg/...` paths in the config.

## Development

```bash
go test -race ./...          # unit and integration tests, no Docker needed
docker compose up -d         # fake services on ports 8081-8084, to try hcagg by hand
```
## Releasing

Every change goes through a pull request. Releases are made by tagging `main`.

1. Create a branch, commit, and push:

   ```bash
   git switch main && git pull
   git switch -c fix-timeout-log
   # make changes, run go test -race ./...
   git add .
   git commit -m "Improve timeout error message"
   git push -u origin fix-timeout-log
   ```

2. Open a pull request into `main`. You can open it as a draft early on.

3. When CI is green, merge the PR. CI runs once more on `main`. Nothing is published yet. You can merge several PRs before releasing.

4. Tag `main` with the new version and push the tag:

   ```bash
   git switch main && git pull
   git tag v1.1.0
   git push origin v1.1.0
   ```

5. The tag starts the Release workflow. It runs CI again, then publishes:
   - the `hcagg-linux-amd64` binary and `checksums.txt` as a [GitHub Release](https://github.com/defilippomattia/hcagg/releases), with notes generated from the merged PRs;
   - the image `ghcr.io/defilippomattia/hcagg`, tagged `1.1.0`, `1.1` and `latest`.