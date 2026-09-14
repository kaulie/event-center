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

## Reachability

- `BIND=127.0.0.1` (default): reachable on the host only — use an SSH tunnel
  (`ssh -L 9099:127.0.0.1:9099 cloud-server`) or an nginx vhost.
- `BIND=0.0.0.0`: directly reachable on `:9099`; make sure the cloud firewall /
  security group and `EVENTD_ADMIN_TOKEN` are both in place first.

GitHub webhooks require a **public HTTPS** endpoint, so exposing this service to
GitHub means adding an nginx vhost in front of `127.0.0.1:9099` and pointing the
GitHub webhook at `http(s)://<host>/github-events-ingress` (`/webhooks/github`
remains as an alias for existing hooks).

## Rollback

The whole deployment is a single static binary plus a SQLite file, and nothing
pre-existing on the host is touched:

```bash
# stop, then drop in a previous eventd binary and start again
ssh cloud-server 'systemctl stop event-center'
scp ./eventd.previous cloud-server:/opt/event-center/eventd
ssh cloud-server 'systemctl start event-center'
```

Keep a copy of the binary you are replacing before you overwrite it. **Never
delete** `/opt/event-center/data` — that directory is the event log, and it is
the only thing here that is not reproducible from git. Move it aside if you
ever need a clean slate:

```bash
ssh cloud-server 'mv /opt/event-center/data /opt/event-center/data.$(date +%s).bak'
```
