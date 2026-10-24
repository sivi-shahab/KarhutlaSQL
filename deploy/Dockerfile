# syntax=docker/dockerfile:1
# Build dari root repo: make docker (atau docker build -f deploy/Dockerfile .)
# Varian DuckLake (Fase 8a, ADR 35): make docker-lake (--target lake).

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

# Varian lake: binary dinamis (dlopen) + ekstensi DuckLake/Postgres/httpfs
# di-bundle saat build, sehingga runtime tidak pernah INSTALL dari internet
# (guardrail G-07 menolaknya). DUCKDB_VERSION = versi DuckDB di go.mod.
FROM golang:1.27-bookworm AS build-lake
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY pkg pkg
ARG VERSION=dev
ARG DUCKDB_VERSION=v1.5.6
ARG DUCKDB_EXTENSIONS_URL=http://extensions.duckdb.org
RUN CGO_ENABLED=1 go build -tags duckdb_arrow -trimpath \
      -ldflags "-s -w -X github.com/bankmega/karhutlasql/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/... \
 && mkdir /out/data \
 && plat="linux_$(go env GOARCH)" && ext="/out/ext/${DUCKDB_VERSION}/${plat}" && mkdir -p "$ext" \
 && for e in ducklake postgres_scanner httpfs; do \
      curl -fsSL "${DUCKDB_EXTENSIONS_URL}/${DUCKDB_VERSION}/${plat}/${e}.duckdb_extension.gz" | gunzip > "$ext/${e}.duckdb_extension"; \
    done

FROM gcr.io/distroless/cc-debian12:nonroot AS lake
COPY --from=build-lake /out/karhutlasql-server /out/karhutlasql-client /usr/local/bin/
COPY --from=build-lake --chown=nonroot:nonroot /out/ext /home/nonroot/.duckdb/extensions
COPY --from=build-lake --chown=nonroot:nonroot /out/data /data
VOLUME /data
EXPOSE 31337 9090
ENTRYPOINT ["/usr/local/bin/karhutlasql-server"]

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/karhutlasql-server /out/karhutlasql-client /usr/local/bin/
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME /data
EXPOSE 31337 9090
# Password lewat env KARHUTLASQL_PASSWORD (wajib, tidak ada default).
# TLS self-signed di memori untuk dev; produksi: --tls-cert/--tls-key (Helm).
ENTRYPOINT ["/usr/local/bin/karhutlasql-server"]
CMD ["--database-filename", "/data/warehouse.duckdb", "--username", "admin", "--tls-self-signed"]
