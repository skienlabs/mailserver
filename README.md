# Skien Mails

Email for your organisation, built from scratch: a complete mail server, webmail and admin console in **one small Docker container**.

- **Mail server**: receives mail on port 25, delivers outgoing mail itself (or through a relay), and lets apps send through ports 587/465.
- **Trusted mail**: every outgoing message is DKIM-signed. Incoming mail is checked with SPF, DKIM and DMARC, reverse DNS and blocklists. Spam goes to the Spam folder, and forged mail claiming to be from your own domain is rejected.
- **Webmail**: a Material-style app with light and dark themes. It has conversations, search (`from:`, `has:attachment`, `is:unread`…), stars, archive, rich-text compose with attachments, drafts that save themselves, undo send, signatures, out-of-office replies, keyboard shortcuts and live updates.
- **Admin console**: users, groups (info@, team@ with "send as"), domains with DNS record checks, the delivery queue, an activity log, mail traffic and server settings.
- **Installable app**: works on Windows, macOS, Android and iPhone, with an unread badge, new-mail notifications and `mailto:` handling.
- **Small and fast**: one Go binary with SQLite (WAL) and full-text search. It idles at about 30 MB of RAM, and the inbox loads in about 10 ms even under load.

| Inbox (dark) | Inbox (light) |
|---|---|
| ![Inbox in dark mode](docs/inbox-dark.png) | ![Inbox in light mode](docs/inbox-light.png) |
| **Admin console: DNS records** | **Phone** |
| ![DNS setup](docs/admin-dns-dark.png) | <img src="docs/mobile-dark.png" alt="Mobile" width="240"> |

```
┌────────────────────────── skienmail container ──────────────────────────┐
│  :25  inbound MX ──► SPF/DKIM/DMARC + spam score ──► mailbox (SQLite+.eml)  │
│  :587/:465 submission (auth) ─┐                                           │
│  :8080 web app + JSON API ────┴─► local delivery / DKIM sign ─► queue ─► internet │
│                                   retries for 4 days, bounces on failure   │
│  /data  skienmail.db · mail/ · queue/ · uploads/ · avatars/ · certs/       │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## 1. Before you start: the server

| Need | Why |
|---|---|
| A Linux server with Docker and a **static public IP** | Other mail servers connect to it |
| **Inbound port 25** open in your firewall or security group | To receive mail |
| **Outbound port 25** allowed by your provider | To deliver mail directly. **Google Cloud, AWS and Azure block it.** On those, use a relay (step 6). After setup, *Admin console → Overview → Test port 25* tells you which case you're in. |
| Ports 587 and 465 (optional) | Only for phones, printers or scripts that send mail through the server |
| Reverse DNS (PTR) for the IP | Gmail and Outlook reject mail from IPs without it. You set it at your hosting provider. |

## 2. Install

```bash
git clone https://github.com/skienlabs/mailserver.git
cd mailserver
docker compose up -d --build
docker logs skienmail 2>&1 | grep "setup code"
```

The CI workflow (`.github/workflows/build.yml`; if your copy has it at `docs/github-workflow-build.yml`, move it there first) runs the tests and publishes a ready image on every push to `main`. To use it, replace `build: .` in `docker-compose.yml` with `image: ghcr.io/skienlabs/mailserver:latest`.

## 3. HTTPS with Nginx Proxy Manager

Add a **Proxy Host**:

- Domain: `mail.skienlab.com`
- Scheme `http`, forward host `skienmail` if Nginx Proxy Manager is on the same Docker network (see the commented `networks:` in `docker-compose.yml`), otherwise the server's IP. Port `8080`.
- SSL tab: request a Let's Encrypt certificate, then turn on **Force SSL** and **HTTP/2**.
- Advanced tab (recommended, so live updates arrive instantly):
  ```nginx
  proxy_buffering off;
  proxy_read_timeout 1h;
  client_max_body_size 40m;
  ```

Open `https://mail.skienlab.com`. The setup wizard asks for:

1. Your organisation name, the **email domain** (the part after `@`, e.g. `skien.com`) and the **web address** (`mail.skienlab.com`).
2. Your administrator account, e.g. `asi@skien.com`.
3. The setup code from the server log.

> The email domain and web address can differ. Addresses can be `@skien.com` while the server lives at `mail.skienlab.com`, because the MX record connects them.

## 4. DNS records

*Admin console → Domains & DNS* lists every record with your real values and copy buttons. **Check DNS** verifies them live. For `skien.com` served from `mail.skienlab.com` the records look like this:

| Type | Name | Value |
|---|---|---|
| A | `mail.skienlab.com` | your server IP |
| MX | `skien.com` | `10 mail.skienlab.com` |
| TXT (SPF) | `skien.com` | `v=spf1 mx a:mail.skienlab.com -all` |
| TXT (DKIM) | `smYYYYMM._domainkey.skien.com` | `v=DKIM1; k=rsa; p=…` (copy it from the console) |
| TXT (DMARC) | `_dmarc.skien.com` | `v=DMARC1; p=quarantine; adkim=r; aspf=r` |
| PTR | your server IP | `mail.skienlab.com` (set at the hosting provider) |

After that, send a message to a Gmail address and choose *Show original* in Gmail. You should see **SPF: PASS, DKIM: PASS, DMARC: PASS**.

## 5. Add your team

*Admin console → Users → Add user*. Skien Mails creates a temporary password and gives you a ready-to-share message:

```
Sign in at:  https://mail.skienlab.com
Email:       sajin@skien.com
Password:    Kiva-7Rp2-Tumo-x9Qa
```

Each employee must choose their own password the first time they sign in. After that they can manage their profile, photo, signature, password, active sessions and out-of-office reply in **Settings**.

**Groups** (`info@`, `team@`, `support@`) deliver to several people. Members you allow can also send as the group from the **From** menu in compose.

### Installing the app on Windows

Open `https://mail.skienlab.com` in Edge or Chrome and click **Install** (the icon at the right end of the address bar). You can also open the account menu and choose **Install Skien Mails app**. It gets its own window and taskbar icon, shows an unread badge, and handles `mailto:` links.

## 6. Outgoing relay (only if port 25 is blocked)

Go to *Admin console → Server settings → Outgoing relay*. Enter your relay provider's host, port, username and password: for example Amazon SES (`email-smtp.ap-south-1.amazonaws.com:587`), Brevo (`smtp-relay.brevo.com:587`) or Mailgun. Then add the provider's `include:` to your SPF record, e.g. `v=spf1 mx include:amazonses.com -all`.

## 7. TLS certificate for SMTP

Mail between servers is always encrypted with STARTTLS. Without configuration the server uses a self-signed certificate, which other mail servers accept. Phones and desktop mail apps connecting on 587/465 need a real certificate, so reuse the one Nginx Proxy Manager already renews:

```yaml
    environment:
      SM_TLS_CERT: /certs/live/npm-1/fullchain.pem   # npm-N = the certificate ID shown in NPM
      SM_TLS_KEY:  /certs/live/npm-1/privkey.pem
    volumes:
      - /path/to/nginx-proxy-manager/letsencrypt:/certs:ro
    user: "0:0"   # Let's Encrypt keys are readable by root only
```

Renewed certificates are picked up automatically within a minute. If Nginx Proxy Manager doesn't run on the same server, set `SM_ACME=true`, publish ports 80 and 443 instead of 8080, and Skien Mails gets its own Let's Encrypt certificate.

## Configuration (environment variables)

| Variable | Default | Meaning |
|---|---|---|
| `SM_DATA_DIR` | `/data` | Where everything is stored |
| `SM_HOSTNAME` | (from setup) | Public name of the server. Overrides the setting in the console. |
| `SM_HTTP_ADDR` | `:8080` | Web app listener (`off` to disable) |
| `SM_SMTP_ADDR` / `SM_SUBMISSION_ADDR` / `SM_SUBMISSIONS_ADDR` | `:25` / `:587` / `:465` | SMTP listeners (`off` to disable) |
| `SM_TLS_CERT`, `SM_TLS_KEY` | | PEM certificate and key for SMTP TLS |
| `SM_ACME`, `SM_ACME_EMAIL` | `false` | Built-in Let's Encrypt (serves HTTPS on `:443` itself) |
| `SM_TRUST_PROXY` | `true` | Trust `X-Forwarded-*` headers from private-network proxies |
| `SM_SETUP_TOKEN` | random | Fixed setup code, if you prefer |
| `SM_MAX_MESSAGE_MB` | `25` | Default message size limit |
| `SM_DEBUG` | `false` | Verbose logging |

## Operations

```bash
# Locked out? Give any account (including the admin) a new temporary password
docker exec skienmail skienmail reset-password asi@skien.com

# Backup: a consistent database snapshot plus the message files
docker exec skienmail skienmail backup /data/backup-$(date +%F).db
docker run --rm -v mailserver_skienmail-data:/data -v "$PWD":/out alpine \
  tar czf /out/skienmail-$(date +%F).tgz -C /data backup-$(date +%F).db mail avatars

# Update to a new version (your data is in the volume and is kept)
git pull && docker compose up -d --build
```

Check `docker volume ls` for the exact volume name. Restoring means stopping the container, putting `skienmail.db` (renamed from the backup), `mail/` and `avatars/` back into the volume, and starting it again.

## Security notes

- Passwords are hashed with Argon2id. Sign-in is rate-limited per IP and per account. Sessions are HttpOnly, SameSite cookies, and you can see and revoke them in Settings.
- The API requires a custom header and a same-origin check (CSRF protection), and the web app runs under a strict Content-Security-Policy.
- HTML email is sanitised on the server and shown in a sandboxed frame with its own CSP. Scripts never run, and remote images (trackers) stay blocked until you choose **Show images**.
- The server is never an open relay: port 25 only accepts mail for your domains, and submission requires authentication and only lets you send as yourself or as groups you're allowed to use.
- The container runs as an unprivileged user (except when you opt into reading root-owned certificates).

## Development

```bash
go test ./...                       # end-to-end tests: setup, users, SMTP in/out, threading, search, bounces…
SM_DATA_DIR=./data SM_SMTP_ADDR=:2525 SM_SUBMISSION_ADDR=:2587 SM_SUBMISSIONS_ADDR=:2465 go run ./cmd/skienmail
```

The frontend is plain ES modules in `web/static`, with no build step, embedded into the binary. `tools/gen_icons.py` and `tools/gen_logo.py` regenerate the icon sprite and app icons.

**Credits:** Google Sans font (SIL Open Font License) and Material Design Icons (Apache 2.0). Their licences are in `third_party/`.
