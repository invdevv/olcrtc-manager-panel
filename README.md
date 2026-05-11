# olcrtc-manager-panel

Web panel and process manager for running multiple `olcrtc` server instances.

Version 2 includes:

- admin panel at `/admin`;
- first-run password setup;
- client creation, edit, delete, room/key rotation, restart, logs, QR, subscription export;
- per-client subscriptions at `/<client-id>/`;
- traffic quota metadata in subscriptions;
- automatic incoming traffic accounting;
- traffic limit and expiration blocking;
- speed limits through per-client `network namespace` + `veth`;
- one isolated `olcrtc` process per client location.

## Requirements

The manager must run on Linux with root privileges because v2 creates network namespaces, veth interfaces, routes, iptables rules, and `tc` qdisc limits.

Required tools on the server:

```sh
ip
iptables
tc
systemctl
```

Runtime files expected by the default systemd unit:

- `/usr/local/bin/olcrtc-manager`
- `/usr/local/bin/olcrtc`
- `/etc/olcrtc-manager/config.json`
- optional `/etc/olcrtc-manager/panel.env`

Do not create `panel.env` for a fresh install if you want first-run setup. The panel will ask you to create the admin password on first open.

## Build

Build frontend assets first, then build the Go binary so the panel is embedded into the manager:

```sh
pnpm install
pnpm build
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o olcrtc-manager ./cmd/olcrtc-manager
```

If you only change Go code and `cmd/olcrtc-manager/web/dist` is already present, `go build` is enough.

## Install

Copy binaries and config:

```sh
sudo install -m 0755 olcrtc-manager /usr/local/bin/olcrtc-manager
sudo install -m 0755 olcrtc /usr/local/bin/olcrtc
sudo install -d -m 0755 /etc/olcrtc-manager
sudo install -m 0600 config.json /etc/olcrtc-manager/config.json
```

Install and start the systemd service:

```sh
sudo install -m 0644 packaging/systemd/olcrtc-manager.service /etc/systemd/system/olcrtc-manager.service
sudo systemctl daemon-reload
sudo systemctl enable --now olcrtc-manager
```

Check status:

```sh
sudo systemctl status olcrtc-manager
sudo journalctl -u olcrtc-manager -f
```

The manager listens on `127.0.0.1:<config.port>`. The default examples use port `8888`.

## First Run

Open the panel:

```text
http://SERVER:8888/admin
```

If `/etc/olcrtc-manager/panel.env` does not exist or does not contain a password, the panel starts in first-run mode and asks you to set the admin password.

After setup, the manager writes:

```sh
/etc/olcrtc-manager/panel.env
```

Example content:

```sh
OLCRTC_MANAGER_USER='admin'
OLCRTC_MANAGER_PASS='your-password'
```

The panel then uses cookie sessions for login. You can change the password later from the `Пароль` button in the panel header.

## Reverse Proxy

The manager binds to `127.0.0.1` by default. To publish it through nginx:

```nginx
server {
    listen 9443 ssl http2;
    server_name example.com;

    ssl_certificate /path/fullchain.pem;
    ssl_certificate_key /path/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8888;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Then open:

```text
https://example.com:9443/admin
```

## Config

Minimal config:

```json
{
  "version": 1,
  "name": "OlcRTC VPS",
  "port": 8888,
  "clients": [
    {
      "client-id": "default",
      "quota": {
        "speed_mbps": 10,
        "traffic_gb": 100,
        "expires_at": "2026-12-31"
      },
      "locations": [
        {
          "name": "Current VPS",
          "endpoint": {
            "room_id": "room-01",
            "key": "e830d36f7be8cfb04a741fc1a5e2ddf8ff04f30985dc070616483f939ad5fafe"
          },
          "carrier": "wbstream",
          "transport": {
            "type": "datachannel"
          },
          "link": "direct",
          "data": "data",
          "dns": "1.1.1.1:53"
        }
      ]
    }
  ]
}
```

Quota fields:

- `speed_mbps`: speed limit for the client location. `0` or absent means unlimited.
- `traffic_gb`: traffic limit. `0` or absent means unlimited.
- `used_bytes`: automatically updated by the manager.
- `used_gb`: derived/legacy display value.
- `expires_at`: optional expiration date in `YYYY-MM-DD`.

The old top-level `locations` format is still accepted and normalized to `clients`.

`endpoint.room_id` must be concrete. `any` is rejected.

## Network Isolation And Limits

For each running location the manager creates:

- network namespace: `olc-*`;
- host veth: `olh*`;
- namespace veth: `oln*`;
- NAT rule for namespace egress;
- DNS file at `/etc/netns/<namespace>/resolv.conf`;
- optional `tc tbf` speed limit on both veth sides.

Useful checks:

```sh
ip netns list
ip -br link | grep olh
tc qdisc show dev olhXXXXXXXX
ip netns exec olc-XXXXXXXX tc qdisc show
iptables -t nat -S POSTROUTING | grep olcrtc-manager-netns
```

Traffic accounting uses the host veth `tx_bytes`, which represents traffic sent from the VPS toward the client namespace. When the configured traffic quota is exceeded, the manager stops that client's location. If you increase `traffic_gb` above `used_bytes`, reload/restart will start it again.

## Subscriptions

Client subscription:

```text
http://127.0.0.1:8888/<client-id>/
```

The subscription includes quota metadata when configured:

```text
#quota-speed-mbps: 10
#quota-traffic-gb: 100
#quota-used-gb: 5
#quota-used-bytes: 5368709120
#quota-expires-at: 2026-12-31
#quota-status: active
```

Possible quota statuses:

- `active`
- `expired`
- `traffic_exceeded`

## Reload

Reload config and apply changed clients without restarting unchanged processes:

```sh
sudo systemctl reload olcrtc-manager
```

Or locally:

```sh
curl -X POST http://127.0.0.1:8888/-/reload
```

## API And Panel Auth

On a fresh install there is no default password. First-run setup must be completed from `/admin`.

After setup:

- UI login uses a cookie session.
- Basic auth still works for scripts and curl.
- Password can be changed from the panel.

## Helper Scripts

Small helper scripts are available in `scripts/` for editing the JSON config:

```sh
scripts/add-user.sh /etc/olcrtc-manager/config.json alice --from default
scripts/modify-user.sh /etc/olcrtc-manager/config.json alice --location-name Germany --room-prefix alice-room
scripts/delete-user.sh /etc/olcrtc-manager/config.json alice
```

Pass `--reload http://127.0.0.1:8888/-/reload` to reload the running manager after saving the config.
