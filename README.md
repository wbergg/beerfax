# beerfax

Daily beer-roll summary, rendered to TIFF and queued as an Asterisk fax.

Pulls the in-progress 04:00→04:00 day window from
`https://beer.wberg.com/api/public/roll`, builds an ASCII summary
(leaderboard, vetoes, ABV table, hour histogram, streaks, country tour,
per-user details), renders it through ImageMagick to a fax-compliant TIFF,
and either queues it through Asterisk or just writes it to disk.

Every mode also renders a viewable PDF alongside the TIFF, and — when the
`email` config block is present — mails it as a report through the local
`msmtp`: the report text in the message body with the PDF attached. Dry-run
emails get a `[DRY-RUN]` subject prefix, `--date` replays a `(replay)` suffix.

## Build

```
go build ./...
```

No external Go dependencies — pure stdlib.

## Run

```
./beerfax --config config.json              # full pipeline (queue fax)
./beerfax --config config.json --dry-run    # render TIFF + PDF, no archive, no queue
./beerfax --config config.json --date 2026-04-29   # replay a fixed day, archive TIFF + PDF, no queue
```

`config.json` (see `config.example.json`):

```json
{
  "api_url": "https://beer.wberg.com/api/public/roll",
  "telephony": {
    "dest_ext": 2003,
    "caller_id": "\"BRD LIVIN Daily\" <beerfax>",
    "from_name": "beerfax"
  },
  "email": {
    "to": ["recipient@example.com"],
    "from": "beerfax <sender@example.com>"
  },
  "fax_spool_path": "/var/spool/asterisk/outgoing",
  "fax_storage_path": "/var/lib/beerfax/storage"
}
```

The `telephony` block sets the Asterisk destination extension (`dest_ext`),
the fax caller ID, and the from-name stamped on the fax header.

The `email` block is optional: omit it (or leave `to` empty) and no email is
sent. `from` is also optional — msmtp's own config supplies the envelope
sender. Delivery uses whatever account is configured in `~/.msmtprc`. In live
mode an email failure still leaves the fax queued (exit code 8 flags it).

The process needs write access to `fax_storage_path`, to its sibling
`<storage>/../beerfax/archive/`, and (for the live path) to `fax_spool_path`.

## Runtime requirements

- Linux (the call-file path chowns to the local `asterisk` user).
- ImageMagick — `convert` must be on `PATH`.
- A Courier font installed for ImageMagick (`-font Courier` is hard-coded).
- Network egress to `beer.wberg.com`.
- Live mode only: a running Asterisk with PJSIP extension `2003` configured
  to receive faxes, and an `asterisk` system user.
- Email only: `msmtp` on `PATH` with a working `~/.msmtprc`.

Ghostscript is **not** required — beerfax only takes the PNG→TIFF path.

## Modes at a glance

| mode             | needs ImageMagick | needs network | needs Asterisk | emails PDF* |
|------------------|:-:|:-:|:-:|:-:|
| `--dry-run`      | yes | yes | no | yes |
| `--date YYYY-MM-DD` | yes | yes | no | yes |
| (default)        | yes | yes | yes | yes |

\* only when the `email` config block has recipients.
