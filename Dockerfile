FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w" -trimpath -o /out/netbird-dns-sync ./cmd

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/netbird-dns-sync /usr/local/bin/netbird-dns-sync

CMD [ "/usr/local/bin/netbird-dns-sync" ]
