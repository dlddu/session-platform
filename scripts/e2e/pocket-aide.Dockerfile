# Packs one statically linked binary — PocketAide (server or oidcmock) or drop — that
# scripts/e2e/up.sh built from its pinned commit (dlddu/pocket-aide, dlddu/drop).
FROM gcr.io/distroless/static-debian12:nonroot
ARG BINARY
COPY ${BINARY} /usr/local/bin/${BINARY}
USER nonroot:nonroot
