# Changelog

Skien Mails uses simple version numbers: **1.0.1, 1.0.2 …** for fixes and small additions, **1.1.0** for bigger features. The running version is shown at the bottom of the side menu, in *Admin console → Overview*, and by `docker exec skienmail skienmail version`.

## 1.1.0 — 18 October 2026

- **Mail apps (IMAP)**: read and manage mail in Outlook, Apple Mail, Thunderbird and phone mail apps, on ports 993 (SSL/TLS) and 143 (STARTTLS). Folders, read status, stars, moves and deletes stay in sync with the web app, new mail shows up right away (IDLE), and *Settings → Email apps* lists the exact settings to type in. Mail apps need a real TLS certificate (README step 7). New settings `SM_IMAPS_ADDR` and `SM_IMAP_ADDR`; publish ports 993 and 143 in `docker-compose.yml` and the firewall.
- **Your own folders**, including subfolders (`Clients/Acme`): create them from the side menu, move conversations with **Move to** (or `v`), rename and delete. They also appear in mail apps, in imports and in exports.
- **Filters**: sort new mail as it arrives by sender, recipient, subject or words, then move it, mark it read or star it. Make one from any message (⋮ → *Filter messages like this*) and optionally apply it to the mail you already have.
- **Blocked and always-allowed senders**, by address or whole domain: blocked mail goes to Spam, allowed senders skip the spam filter when their mail is verified as really coming from their domain (SPF or DKIM), so forged copies still go to Spam.
- **Notifications when the app is closed** (*Settings → General*): new-mail notifications on each phone or computer you turn them on for, also on iPhone and iPad once Skien Mails is on the Home Screen. End-to-end encrypted, grouped into one notification per burst, never for spam or mail you've already read, and they stop when you sign out of that device (changing your password stops them on all your other devices). Optionally without sender and subject.
- **Export your mail** (*Settings → Import & export*): a .zip with one folder per mail folder and one .eml file per message (drafts saved in mail apps included), plus a list of all messages. It can be imported back here or into another service.
- Importing keeps your own folder names from Zoho, Gmail and other accounts (as new folders) instead of putting everything in Archive.
- Keyboard shortcuts now also work right after ticking a checkbox in the list.

## 1.0.1 — 18 October 2026

- **Import mail** (*Settings → Import mail*): bring over old mail straight from Zoho (company or personal accounts, India / US / Europe data centres), Gmail, Yahoo or any IMAP server, keeping folders, read status, stars and original dates. You can also upload a Zoho export (.zip), an .mbox (Google Takeout, Thunderbird) or an .eml file. Large uploads resume after a dropped connection; running an import again skips messages that are already there.
- **Faster, sturdier outgoing mail queue**: up to 16 deliveries run in parallel (4 at a time per destination), so one slow or unreachable server never holds up mail to everyone else. Every delivery attempt is recorded, and nothing is sent twice even under heavy load. Adjustable with `SM_QUEUE_WORKERS` and `SM_QUEUE_PER_DOMAIN`.
- **Delivery queue screen** (*Admin console → Delivery queue*): live counts (in queue, delivered and failed in the last 24 hours, oldest waiting), what's waiting per destination, filters (in queue / retrying / failed / delivered), search, **Retry all now**, and a detail view per message with every attempt, the receiving server's reply and the headers.
- Importing is protected against broken or hostile mail servers and files (size, memory and time limits throughout).
- Profile photos now show in the top bar and everywhere else as soon as the app opens.
- Version number shown in the side menu and the admin overview.
- Faster search on large mailboxes (bigger database cache, adjustable with `SM_DB_CACHE_MB`).

## 1.0.0 — 17 October 2026

First release in production at mail.skienlab.com.

- Mail for the whole organisation: inbox, conversations, search, labels for starred/important, drafts, undo send, out-of-office replies, groups (info@, support@), people directory.
- Admin console: users, groups, domains with ready-to-copy DNS records (SPF, DKIM, DMARC) and live checks, delivery queue, activity log, server settings.
- Sending and receiving with DKIM signing, SPF/DKIM/DMARC checks, spam scoring, retries and bounce messages.
- Installable app for Windows, Mac and phones; light theme with a soft colour backdrop and an AMOLED-black dark theme.
- Security and performance hardening from a full QA round (Bcc privacy, MIME-bomb protection, search index integrity, 10–100× faster lists and search).
