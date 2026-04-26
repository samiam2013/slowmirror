# slowmirror
calmer weight loss tracking

## Overview

HTTP server that accepts weight readings, stores them in SQLite, and serves a live dashboard via Server-Sent Events. The dashboard shows an exponential moving average (EMA) over the last 14 days and a trend arrow based on recent direction.

Multiple users are supported. When a weight is reported, the server automatically assigns it to the user whose most recent weight is closest to the reported value (within 10%), falling back to unassigned if no match is found within the last 30 days.

## Setup

Users are seeded from `default_users.json` at startup. Edit this file before first run to set names, emails, and starting weights:

```json
{
  "users": [
    { "name": "sam", "email": "sam@example.com", "weight_kg": 90.0 }
  ]
}
```

## Running

### As a system service

```bash
sudo cp slowmirror.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now slowmirror
```

The service builds the binary from source on start. Logs are appended to `slowmirror.log` in the repo root.

### Manually

```bash
go build -o slowmirror .
./slowmirror
```

The server listens on `:8080`. Open `http://localhost:8080` for the live dashboard.

## Reporting a weight

```bash
curl -X POST http://localhost:8080/report -d "kg=90.5"
```

## Importing historical data

Place a `weight_historical.json` export (Apple Health format) in the repo root, then:

```bash
go run ./cmd/import
```

This imports `weight_body_mass` entries (in lbs, converted to kg) into the database.

## Files

| File | Description |
|------|-------------|
| `slowmirror.db` | SQLite database (gitignored) |
| `slowmirror.log` | Service log output (gitignored) |
| `default_users.json` | User seed data loaded at startup |
| `dashboard.html` | Frontend served at `/` |
| `weight_historical.json` | Historical import source (gitignored) |
