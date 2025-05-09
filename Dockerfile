FROM docker.io/library/debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY ./build/netbird-dns-sync /usr/local/bin/netbird-dns-sync

CMD [ "/usr/local/bin/netbird-dns-sync" ]
