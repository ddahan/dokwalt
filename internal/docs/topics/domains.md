# Domains & HTTPS

*Point DNS at your server, attach a domain to a service, and HTTPS is set up automatically.*

Every public site in DokWalt is an explicit domain attached to one service
and one container port. Caddy (the `dokwalt-caddy` container) is the only
thing listening on ports 80 and 443. It routes each domain to the right
containers and obtains and renews certificates by itself.

Each app stage has its own private network `dw-<app>-<stage>`. The daemon
attaches Caddy to every one of them, and Caddy reaches your service by
container name (e.g. `dw-blog-production-green-web-1:3000`), round robin
across replicas. There is no shared proxy network: apps can't reach each
other, whether they are public or not.

## 1. Create the DNS records first

At your DNS provider, create a record for each domain pointing at the
server's public IP:

| Type   | Name                   | Value            | When                   |
|--------|------------------------|------------------|------------------------|
| `A`    | `blog.example.com`     | `203.0.113.10`   | always (IPv4)          |
| `AAAA` | `blog.example.com`     | `2001:db8::10`   | only if IPv6 works     |
| `A`    | `www.blog.example.com` | `203.0.113.10`   | if you want a redirect |

Only publish an `AAAA` record if the server is really reachable over IPv6
on 80 and 443: Let's Encrypt prefers IPv6, and a broken `AAAA` record makes
validation fail even when IPv4 is fine. Check propagation from your computer:

```bash
dig +short A blog.example.com
dig +short AAAA blog.example.com
```

Home server with a changing public IP? Use dynamic DNS; see
`dokwalt docs server`.

## 2. Attach the domain

```bash
dokwalt domains:add blog.example.com                              # defaults
dokwalt domains:add blog.example.com --service web --port 3000    # explicit
```

- `--service` defaults to the only stateless service (not a one-shot job)
  that declares a port. If there are several candidates, or none, the
  command fails and lists the services: pass `--service`.
- `--port` defaults to the service's first declared container port, from
  `ports:` or `expose:` in your compose file. DokWalt removes `ports:` at
  deploy time but remembers the container ports as defaults. With no
  declared port, it routes to 80 and warns you.
- Before the first deploy there is no compose model yet, so `--service` is
  required. Without `--port`, the port is taken from the compose file at
  the first deploy.
- Hostnames are lowercased, a trailing dot is stripped, wildcards are
  rejected, and each hostname can be used only once per server.

**The domain is always added**: there is no confirmation prompt. It gets
routed immediately and HTTP redirects to HTTPS. Then DokWalt checks whether
the domain actually reaches this server and tells you.

### The reachability check

The daemon fetches
`http://<domain>/.well-known/dokwalt-check/probe` (8 s timeout). Caddy
answers that path on port 80 for any host with the server's own check
token. Getting the token back proves that DNS, your router or provider
firewall, and Caddy are all right, so the certificate request will succeed.

The domain is resolved through **public resolvers** (1.1.1.1, then 8.8.8.8),
like Let's Encrypt and your visitors see it. The server's own resolver would
cache "no such name" for a while when you run `domains:add` before creating
the record, and keep reporting it after the record is live. If no public
resolver answers (outbound DNS blocked, or a name that only exists on your
network), the check uses the server's resolver.

```text
$ dokwalt domains:add blog.example.com --service web --port 3000
  blog.example.com → web:3000
✓ blog.example.com → reachable; HTTPS certificate is issued automatically
```

### When the check fails

If the probe fails, a DNS lookup explains why. No record:

```text
$ dokwalt domains:add api.example.com --service api
  api.example.com → api:8080
! api.example.com was added but isn't reachable yet:
  no DNS record yet — create an A (and/or AAAA) record pointing to this server
  Caddy keeps retrying the certificate; check again with dokwalt domains
```

A record exists but port 80 doesn't reach Caddy:

```text
! shop.example.com was added but isn't reachable yet:
  DNS points to 198.51.100.7 but this server's proxy didn't answer on port 80 — check
  the IP, firewall (80/443) and router port forwarding. (Testing from the same LAN? Your
  router may not support hairpin NAT: then this check can't succeed but the site may
  still work from outside.)
  Caddy keeps retrying the certificate; check again with dokwalt domains
```

What to fix:

- **Wrong IP**: fix the `A` record and wait for its TTL.
- **Home**: forward TCP 80 and 443 (and UDP 443) to the server's LAN IP.
- **VPS**: allow 80/tcp, 443/tcp, 443/udp in the provider firewall and ufw.
- **Hairpin NAT**: the check runs on the server, which calls its own public
  IP. Many home routers don't loop that back. Verify from outside, e.g. a
  phone on mobile data: `curl -I http://blog.example.com`. Let's Encrypt
  connects from the internet, so certificates still work.

A failed check is only a warning: the domain stays configured, and Caddy
keeps retrying the certificate on its own.

## 3. Certificates

- **Public names**: Let's Encrypt (ACME), using the email from
  `dokwalt server init --email`. Caddy renews automatically before expiry.
- **Local names**: hostnames ending in `.localhost`, `.test`, `.internal`,
  `.local`, `.lan` or `.home.arpa`, and bare IP addresses, get a certificate
  from Caddy's **internal CA** instead. No Let's Encrypt, and no probe: the
  check reports `ok (local name, internal certificate)`. Browsers don't
  trust that CA until you import its root certificate:
  `ssh prod docker cp dokwalt-caddy:/data/caddy/pki/authorities/local/root.crt .`
- **Storage**: certificates live in the Docker volume `dokwalt-caddy-data`
  (`/data` in the Caddy container). They survive reboots, `server upgrade`
  and Caddy being recreated. Include that volume in server backups.
- **HTTP/3**: Caddy publishes 443/udp. If UDP 443 is blocked, browsers use
  HTTP/2 instead.

### Rate limits

Let's Encrypt limits failed validations per hostname per hour, and
certificates per domain per week. Two things protect you. The reachability
check catches most mistakes before you wait on a certificate. And when
issuance fails, Caddy retries with backoff against the Let's Encrypt
**staging** endpoint until it works again, which spares the production
limits. Fix the cause, then wait: retries are automatic.

## 4. Listing domains

```text
$ dokwalt domains
DOMAIN                  TARGET               REACHABILITY
blog.example.com        web:3000             ✓ ok
www.blog.example.com    → blog.example.com   ✓ ok
api.blog.example.com    api:8080             ! no DNS record yet — create an A (and/or AAAA) record …
```

`dokwalt domains` re-runs the reachability check for every domain.
`--no-check` skips the probes and lists instantly. `--json` for scripts.

## 5. Redirects (www → apex)

```bash
dokwalt domains:redirect www.blog.example.com blog.example.com
```

The source domain needs its own DNS record and gets its own certificate.
The same check runs. Caddy answers with a **308** to `https://<to>{uri}`,
so path and query are kept: `https://www.blog.example.com/a?b=1` →
`https://blog.example.com/a?b=1`. Remove a redirect like any domain:
`dokwalt domains:remove www.blog.example.com`.

## 6. Several domains, several services

Run `domains:add` once per hostname. Each gets its own certificate:

```bash
dokwalt domains:add blog.example.com   --service web
dokwalt domains:add blog.example.org   --service web
dokwalt domains:add admin.example.com  --service admin --port 8080
```

A service with at least one domain is **public**. Only public services get
the HTTP health check during deploys (`dokwalt docs deploy`).

## 7. Domains per stage (pipelines)

With a pipeline enabled, each stage has its own domains:

```bash
dokwalt domains:add blog.example.com                    --service web
dokwalt domains:add staging.blog.example.com -s staging --service web
```

`promote` never moves domains: each stage keeps its own.

## 8. Removing a domain

```bash
dokwalt domains:remove blog.example.com
```

Caddy stops routing it right away, through an atomic config reload that
doesn't affect other sites. Delete the DNS record yourself.

## What visitors see when the app isn't serving

| Situation                        | Response                                          |
|----------------------------------|---------------------------------------------------|
| added before the first deploy    | 503 `<app> is not running yet.`                   |
| after `dokwalt stop`             | 503 `<app> is temporarily unavailable for maintenance.` |

## What is not supported

- **Wildcards** (`*.example.com`) and wildcard certificates: add each host.
- **DNS-01 challenge**: ports 80/443 must reach the server. Behind CGNAT or
  a router you can't configure, see `dokwalt docs server` (Cloudflare
  Tunnel / Tailscale options).

## When something goes wrong

- A failed issuance reported by Caddy fires the **Certificate error** alert
  (`Could not get a certificate for <host>: …`). It resolves when the
  certificate is obtained. See `dokwalt docs alerts`.
- `dokwalt doctor` runs the reachability check for every domain:

```text
$ dokwalt doctor
✓ Domain blog.example.com      ok
! Domain api.blog.example.com  no DNS record yet — create an A (and/or AAAA) record pointing to this server
```

- Caddy's own log (ACME errors, config loads):
  `ssh prod docker logs --since 1h dokwalt-caddy`.

See also: `dokwalt docs deploy`, `dokwalt docs alerts`,
`dokwalt docs troubleshooting`, `dokwalt docs server`
