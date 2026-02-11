FROM docker.io/library/debian:trixie-slim



COPY ./build/netbird-dns-sync /usr/local/bin/netbird-dns-sync

CMD [ "/usr/local/bin/netbird-dns-sync" ]
