# Deploying event-center to cloud-server

Target: `cloud-server` (SSH alias → `115.190.153.53`, CentOS Stream 9).

Follows the convention already used on that host (`/opt/<app>` + a systemd unit,
service bound to loopback and fronted by nginx when it needs to be public).

```
/opt/event-center/eventd             # static linux/amd64 binary
/opt/event-center/data/eventd.db     # SQLite event log (the only writable path)
/opt/event-center/event-center.env   # secrets, root-only 0600, never in git
/etc/systemd/system/event-center.service
```

## Install / upgrade

```bash
cd <repo>
deploy/cloud-server/install.sh                       # → 127.0.0.1:9099 (default)
EC_BIND=0.0.0.0 deploy/cloud-server/install.sh       # exposed on :9099 (open the cloud firewall first)
EVENTD_GITHUB_SECRET=xxx deploy/cloud-server/install.sh   # set/rotate the GitHub webhook secret
```

The installer is idempotent: it rebuilds, uploads, refreshes the unit and
restarts the service. It never touches an existing database, and it never
clears an existing secret — `EVENTD_GITHUB_SECRET` is only written when you
pass it explicitly.

## Secrets

`EVENTD_ADMIN_TOKEN` is generated **on the host** with `openssl rand -hex 32` the
first time and written to `event-center.env` (mode 0600). It is never uploaded
and never committed; the installer prints it once on first creation.

Read it later:

```bash
ssh cloud-server 'grep EVENTD_ADMIN_TOKEN /opt/event-center/event-center.env'
```

Rotate by editing that file and running `systemctl restart event-center`.

> **Why this matters:** with an empty `EVENTD_ADMIN_TOKEN` the admin API is
> unauthenticated — anyone reaching the port could create subscriptions and read
> every event. The generated token prevents that. `EVENTD_GITHUB_SECRET` is only
> needed to accept GitHub webhooks.

## Operating

```bash
ssh cloud-server 'systemctl status event-center --no-pager'
ssh cloud-server 'journalctl -u event-center -n 50 --no-pager'
ssh cloud-server 'curl -s localhost:9099/healthz'
ssh cloud-server 'curl -s localhost:9099/metrics | head'
```

## Logs (ingress audit trail)

Two complementary sinks:

| Sink | What | Retention |
|---|---|---|
| journald | all service logs, incl. one line per ingest attempt | current boot only — see the warning below |
| `/var/log/event-center/ingress.jsonl` | one JSON line per ingress attempt (accepted / duplicate / rejected / error) | 0600, logrotate: daily, 100M cap, 90 rotations, compressed |

```bash
ssh cloud-server 'journalctl -u event-center -n 50 --no-pager'
ssh cloud-server 'grep "\"outcome\":\"rejected\"" /var/log/event-center/ingress.jsonl | tail'
ssh cloud-server 'grep "\"request_id\":\"<github-delivery-id>\"" /var/log/event-center/ingress.jsonl'
```

The unit disables journald rate limiting (`LogRateLimitIntervalSec=0`): audit
lines are evidence and must not be silently dropped under a traffic burst.
logrotate uses `copytruncate` because the service keeps the file open — do not
change that to `create`, or the process would keep writing to the rotated inode.

> **journald on this host is volatile.** `/var/log/journal` does not exist, so
> `journalctl` history is lost on reboot. That is why the audit file exists as a
> real, rotating file. Enabling persistent journald is a host-wide change and is
> deliberately left to you:
>
> ```bash
> ssh cloud-server 'mkdir -p /var/log/journal && systemd-tmpfiles --create --prefix /var/log/journal && systemctl restart systemd-journald'
> ```

To also capture the raw payload of *accepted* requests (rejected ones are always
captured), set `EVENTD_INGRESS_LOG_BODY=true` in `event-center.env`.

## Public HTTPS edge (nginx)

The service listens on loopback; nginx terminates TLS and is the only public
entry point. Set it up once with:

```bash
deploy/cloud-server/setup-nginx.sh                       # → event-center.<ip>.sslip.io
EC_TLS_HOST=events.example.com deploy/cloud-server/setup-nginx.sh   # own domain
```

It adds two server blocks (**it never modifies other sites' configuration**):

| File | Purpose |
|---|---|
| `/etc/nginx/conf.d/event-center-acme.conf` | HTTP-01 challenge on :80, kept for renewals |
| `/etc/nginx/conf.d/event-center.conf` | :443 TLS, allowlist on the ingest path, proxy to `127.0.0.1:9099` |

What the edge enforces:

- **TLS** with a Let's Encrypt certificate (auto-renewed by the existing
  certbot timer). The webhook URL becomes
  `https://event-center.115-190-153-53.sslip.io/github-events-ingress`.
- **GitHub-only ingestion**: `/github-events-ingress` allows only GitHub's
  published hook ranges (`140.82.112.0/20`, `143.55.64.0/20`, `192.30.252.0/22`,
  `185.199.108.0/22` + two IPv6 ranges) plus `127.0.0.1` for testing on the box.
  This is the compensating control for the deliberately weak HMAC secret: a
  guessed secret is useless from an address GitHub never sends from.
- **`/metrics` localhost-only** — the application serves it unauthenticated (it
  is built for a local scraper), so the restriction lives in the edge.
- Everything else is proxied as-is and authenticated by the application
  (admin token / subscription API key), with `X-Forwarded-For` set so the
  ingress audit trail records the real caller.

Verify from outside:

```bash
curl -sS https://event-center.115-190-153-53.sslip.io/healthz        # 200
curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
  https://event-center.115-190-153-53.sslip.io/github-events-ingress # 403 (not a GitHub address)
curl -sS -o /dev/null -w '%{http_code}\n' \
  https://event-center.115-190-153-53.sslip.io/metrics               # 403
```

## The GitHub webhook

Live URL: **`https://115.190.153.53/github-events-ingress`**
(secret: the value of `EVENTD_GITHUB_SECRET`; `insecure_ssl=1`, see below).

### Why the URL uses the IP and not a hostname

This host sits behind a policy that **resets traffic carrying an unfiled domain
name** — it inspects the TLS SNI (and the plain-HTTP `Host` header) and kills the
connection, so `https://<name>.sslip.io` is unreachable from outside while the
literal IP works. Measured:

| Request | Result |
|---|---|
| `http://<ip>/…` with `Host: event-center.<ip>.sslip.io` | connection reset |
| `http://<ip>/…` with `Host: <ip>` | 301 (fine) |
| `https://<ip>/…` (no SNI) | 200 (fine) |
| `https://event-center.<ip>.sslip.io/…` (SNI = hostname) | connection reset |

The vhost therefore serves both names, but the webhook must use the IP. The
certificate names the sslip host, so a client connecting by IP cannot verify the
name — hence `insecure_ssl=1` on the hook. Payload authenticity does not depend
on it: the HMAC signature over the body is what proves the sender.

> **Certificate renewal will fail while the policy is in place**, because the
> Let's Encrypt HTTP-01 challenge fetches `http://<host>/.well-known/…` with the
> domain as `Host`. The current certificate expires 2026-12-13; since clients
> connect by IP with `insecure_ssl=1`, an expired/self-signed certificate keeps
> working — but do not expect the renewal timer to succeed.

> **Changing the hook: always send the whole `config` object.**
> GitHub's `PATCH /repos/{owner}/{repo}/hooks/{id}` **replaces** `config`, so a
> request containing only `config[url]` silently drops the hook's `secret` and
> resets `content_type` to the legacy form encoding. Deliveries then arrive
> unsigned and are rejected with 401 (`missing signature header`). This actually
> happened while moving the hook. Use:
>
> ```bash
> gh api -X PATCH repos/<owner>/<repo>/hooks/<id> \
>   -f 'config[url]=https://115.190.153.53/github-events-ingress' \
>   -f 'config[content_type]=json' \
>   -f 'config[secret]=<the EVENTD_GITHUB_SECRET value>' \
>   -f 'config[insecure_ssl]=1'
> ```
>
> Verify afterwards with `POST /repos/<owner>/<repo>/hooks/<id>/tests` and check
> that the delivery answered 202 in the hook's *Recent Deliveries*.

## Reachability

- `BIND=127.0.0.1` (default): reachable on the host only — use an SSH tunnel
  (`ssh -L 9099:127.0.0.1:9099 cloud-server`) or an nginx vhost.
- `BIND=0.0.0.0`: directly reachable on `:9099`; make sure the cloud firewall /
  security group and `EVENTD_ADMIN_TOKEN` are both in place first.

GitHub webhooks require a **public HTTPS** endpoint, so exposing this service to
GitHub means adding an nginx vhost in front of `127.0.0.1:9099` and pointing the
GitHub webhook at `http(s)://<host>/github-events-ingress` — the only path that
accepts GitHub deliveries (retired paths are removed, not aliased).

## Reset / archive the event log

The database is the only thing here that git cannot reproduce, so it is moved
aside, never deleted. **Recreate the directory afterwards**: the unit's
`ReadWritePaths` must exist or systemd fails at namespace setup
(`status=226/NAMESPACE`) and the service cannot start at all.

```bash
ssh cloud-server 'systemctl stop event-center'
ssh cloud-server 'mv /opt/event-center/data /opt/event-center/data.$(date -u +%Y%m%dT%H%M%SZ).bak'
ssh cloud-server 'install -d -m 0755 /opt/event-center/data'   # required, see above
ssh cloud-server 'systemctl start event-center'
```

Optionally archive the audit trail at the same time (the service recreates the
file on start, so moving it is safe):

```bash
ssh cloud-server 'mv /var/log/event-center/ingress.jsonl /var/log/event-center/ingress.$(date -u +%Y%m%dT%H%M%SZ).bak.jsonl'
```

After the reset `sources` is empty, but the `github` source is re-seeded from
`EVENTD_GITHUB_SECRET` on the next start; sequences restart at 1.

## Rollback a binary

Keep a copy of the binary you are replacing before you overwrite it:

```bash
ssh cloud-server 'cp /opt/event-center/eventd /opt/event-center/eventd.previous'
# ... then, to go back:
ssh cloud-server 'systemctl stop event-center'
scp ./eventd.previous cloud-server:/opt/event-center/eventd
ssh cloud-server 'systemctl start event-center'
```
