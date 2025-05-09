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