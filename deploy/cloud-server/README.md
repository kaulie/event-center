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
PORT=9099 BIND=0.0.0.0 deploy/cloud-server/install.sh
EVENTD_GITHUB_SECRET=xxx deploy/cloud-server/install.sh   # seed the GitHub source
```

The installer is idempotent: it rebuilds, uploads, refreshes the unit and
restarts the service. It never touches an existing database or secrets file.

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

## Reachability

- `BIND=127.0.0.1` (default): reachable on the host only — use an SSH tunnel
  (`ssh -L 9099:127.0.0.1:9099 cloud-server`) or an nginx vhost.
- `BIND=0.0.0.0`: directly reachable on `:9099`; make sure the cloud firewall /
  security group and `EVENTD_ADMIN_TOKEN` are both in place first.

GitHub webhooks require a **public HTTPS** endpoint, so exposing this service to
GitHub means adding an nginx vhost in front of `127.0.0.1:9099` and pointing the
GitHub webhook at `https://<host>/webhooks/github`.

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
