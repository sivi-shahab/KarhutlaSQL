# syntax=docker/dockerfile:1
# Build dari root repo: make docker (atau docker build -f deploy/Dockerfile .)

FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY pkg pkg
ARG VERSION=dev
# Binary statis penuh (DuckDB static lib + libstdc++) agar jalan di distroless/static.
# Konsekuensi: ekstensi DuckDB tidak bisa di-LOAD (dlopen) di image ini.
RUN CGO_ENABLED=1 go build -tags duckdb_arrow,netgo,osusergo -trimpath \
      -ldflags "-s -w -linkmode external -extldflags -static -X github.com/bankmega/karhutlasql/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/... \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/karhutlasql-server /out/karhutlasql-client /usr/local/bin/
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME /data
EXPOSE 31337 9090
# Password lewat env KARHUTLASQL_PASSWORD (wajib, tidak ada default).
# TLS self-signed di memori untuk dev; produksi: --tls-cert/--tls-key (Helm).
ENTRYPOINT ["/usr/local/bin/karhutlasql-server"]
CMD ["--database-filename", "/data/warehouse.duckdb", "--username", "admin", "--tls-self-signed"]
