# beerfax

Daily beer-roll summary, rendered to TIFF and queued as an Asterisk fax.

Pulls the in-progress 04:00→04:00 day window from
`https://beer.wberg.com/api/public/roll`, builds an ASCII summary
(leaderboard, vetoes, ABV table, hour histogram, streaks, country tour,
per-user details), renders it through ImageMagick to a fax-compliant TIFF,
and either queues it through Asterisk or just writes it to disk.

The non-fax modes (`--dry-run` and `--date`) also save a viewable PDF
alongside the TIFF so you can preview the output without a fax machine.

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
  "fax_spool_path": "/var/spool/asterisk/outgoing",
  "fax_storage_path": "/var/lib/beerfax/storage"
}
```

The `telephony` block sets the Asterisk destination extension (`dest_ext`),
the fax caller ID, and the from-name stamped on the fax header.

The process needs write access to `fax_storage_path`, to its sibling
`<storage>/../beerfax/archive/`, and (for the live path) to `fax_spool_path`.

## Runtime requirements

- Linux (the call-file path chowns to the local `asterisk` user).
- ImageMagick — `convert` must be on `PATH`.
- A Courier font installed for ImageMagick (`-font Courier` is hard-coded).
- Network egress to `beer.wberg.com`.
- Live mode only: a running Asterisk with PJSIP extension `2003` configured
  to receive faxes, and an `asterisk` system user.

Ghostscript is **not** required — beerfax only takes the PNG→TIFF path.

## Modes at a glance

| mode             | needs ImageMagick | needs network | needs Asterisk |
|------------------|:-:|:-:|:-:|
| `--dry-run`      | yes | yes | no |
| `--date YYYY-MM-DD` | yes | yes | no |
| (default)        | yes | yes | yes |
