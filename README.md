# olcrtc-manager

Minimal manager for running multiple `olcrtc` server instances and serving a plain text client subscription.

## Run

```sh
OLCRTC_PATH=./olcrtc ./olcrtc-manager -config ~/.config/olcrtc-manager.json -port 8888
```

Command-line options override config values. Each client subscription is returned from `/<client-id>` on the configured port, for example `http://127.0.0.1:8888/user`.

## Reload

After editing the config, reload the manager without stopping already running unchanged `olcrtc` instances:

```sh
kill -HUP <olcrtc-manager-pid>
```

You can also reload over HTTP from the same host:

```sh
curl -X POST http://127.0.0.1:8888/-/reload
```

Reload compares locations by `client-id`, `endpoint.room_id`, and `transport.type`. New locations are started, removed locations are stopped, and changed locations are restarted. The listen port cannot be changed by reload; restart the manager for that.

## User scripts

Small helper scripts are available in `scripts/` for editing the JSON config. They use `python3` for safe JSON updates:

```sh
scripts/add-user.sh /etc/olcrtc-manager/config.json alice --from user
scripts/modify-user.sh /etc/olcrtc-manager/config.json alice --location-name Germany --room-prefix alice-room
scripts/delete-user.sh /etc/olcrtc-manager/config.json alice
```

Pass `--reload http://127.0.0.1:8888/-/reload` to any script to reload the running manager after saving the config.
`add-user.sh` generates endpoint keys with `openssl rand -hex 32`, including for locations copied with `--from`.
When `--room` is not provided, it generates room ids with `$OLCRTC_PATH -mode gen -carrier ... -dns ... -amount 1`.

## systemd

An example unit is available at `packaging/systemd/olcrtc-manager.service`. It expects:

- `olcrtc-manager` at `/usr/local/bin/olcrtc-manager`
- `olcrtc` at `/usr/local/bin/olcrtc`
- config at `/etc/olcrtc-manager/config.json`
- a dedicated `olcrtc` system user and group

Install it on a Linux host:

```sh
sudo useradd --system --home /var/lib/olcrtc-manager --create-home --shell /usr/sbin/nologin olcrtc
sudo install -m 0755 olcrtc-manager /usr/local/bin/olcrtc-manager
sudo install -m 0755 olcrtc /usr/local/bin/olcrtc
sudo install -d -m 0755 /etc/olcrtc-manager
sudo install -m 0644 olcrtc-manager.json /etc/olcrtc-manager/config.json
sudo install -m 0644 packaging/systemd/olcrtc-manager.service /etc/systemd/system/olcrtc-manager.service
sudo systemctl daemon-reload
sudo systemctl enable --now olcrtc-manager
```

Start, stop, restart, and inspect the service:

```sh
sudo systemctl start olcrtc-manager
sudo systemctl stop olcrtc-manager
sudo systemctl restart olcrtc-manager
sudo systemctl status olcrtc-manager
journalctl -u olcrtc-manager -f
```

After adding or editing users in `/etc/olcrtc-manager/config.json`, reload only the changed `olcrtc` instances:

```sh
sudo systemctl reload olcrtc-manager
```

## Config

```json
{
  "version": 1,
  "name": "ScumVPN",
  "port": 8888,
  "clients": [
    {
      "client-id": "user",
      "locations": [
        {
          "name": "Netherlands",
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
        },
        {
          "name": "Netherlands VP8",
          "endpoint": {
            "room_id": "room-02",
            "key": "e830d36f7be8cfb04a741fc1a5e2ddf8ff04f30985dc070616483f939ad5fafe"
          },
          "carrier": "wbstream",
          "transport": {
            "type": "vp8channel",
            "payload": {
              "vp8-fps": 60,
              "vp8-batch": 64
            }
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

The old top-level `locations` format is still accepted.
`endpoint.room_id` must be concrete. `any` is rejected.
