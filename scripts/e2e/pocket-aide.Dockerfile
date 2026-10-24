# Packs one statically linked PocketAide binary (server or oidcmock) that
# scripts/e2e/up.sh built from the pinned dlddu/pocket-aide commit.
FROM gcr.io/distroless/static-debian12:nonroot
ARG BINARY
COPY ${BINARY} /usr/local/bin/${BINARY}
USER nonroot:nonroot
