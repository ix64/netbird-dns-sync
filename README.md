# Netbird DNS Sync

Sync Netbird internal peer records to real world.

for now, Cloudflare DNS supported only.

## Usage

- Generate [Netbird Access Token](https://docs.netbird.io/how-to/access-netbird-public-api#creating-an-access-token)
- Generate [Cloudflare API Token](https://dash.cloudflare.com/profile/api-tokens)
- Run with docker compose

    ```yaml
    name: netbird
    services:
      dns-sync:
        image: "ghcr.io/ix64/netbird-dns-sync:main"
        restart: unless-stopped
        environment:
          NETBIRD_ENDPOINT: https://netbird.example.com
          NETBIRD_ACCESS_TOKEN: ......
          CLOUDFLARE_DOMAIN: example.com
          CLOUDFLARE_API_TOKEN: ......
    ```

## Configuration

| Environment Variable   | Required | Default | Description                                                    |
|------------------------|----------|---------|----------------------------------------------------------------|
| `NETBIRD_ENDPOINT`     | yes      |         | Netbird Management Endpoint (e.g. `https://netbird.example.com`) |
| `NETBIRD_ACCESS_TOKEN` | yes      |         | Netbird Access Token                                           |
| `CLOUDFLARE_DOMAIN`    | yes      |         | Cloudflare Zone Domain (e.g. `example.com`)                    |
| `CLOUDFLARE_API_TOKEN` | yes      |         | Cloudflare API Token, needs `Zone:Read` and `DNS:Edit`         |
| `SYNC_INTERVAL`        | no       | `15m`   | Sync interval, Go duration format (e.g. `30s`, `5m`, `1h`)     |

Only peers whose DNS label ends with `.<CLOUDFLARE_DOMAIN>` are synced,
so the Netbird DNS domain should be set to the Cloudflare zone (or a subdomain of it).
Other peers are skipped with a warning.
