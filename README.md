# slowmirror
calmer weight loss tracking

## Overview

HTTP server that accepts weight readings, stores them in SQLite, and serves a live dashboard with an exponential moving average (EMA) over the last 14 days via Server-Sent Events.

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

The server listens on `:8080`.

## Reporting a weight

```bash
curl -X POST http://localhost:8080/report -d "kg=108.6"
```

Or edit and run `curl_weight.sh`.

## Importing historical data

Place a `weight_historical.json` export (Apple Health format) in the repo root, then:

```bash
go run ./cmd/import
```

This imports `weight_body_mass` entries from the JSON into `weight.db`.

## Files

| File | Description |
|------|-------------|
| `weight.db` | SQLite database (gitignored) |
| `slowmirror.log` | Service log output (gitignored) |
| `weight_historical.json` | Historical import source (gitignored) |
