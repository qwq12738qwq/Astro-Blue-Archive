# syntax=docker/dockerfile:1
#
# The production image.
#
# Both processes live in ONE container, and that is not a convenience: Go
# honours the forwarded host and the client IP only when the peer is loopback
# (ARCHITECTURE.md ID-24), and Astro proxies every /api/* call to the API. Two
# containers on a bridge network would put the API behind a container address,
# the Origin check would compare the visitor's origin against the API's own
# address and refuse every browser, and the rate limiter would collapse into one
# global bucket.
#
# The TLS terminator is NOT in this image, and neither is a reverse proxy: the
# site owns its security headers and a second proxy would mean a second set of
# them. The site is published on a port; what sits in front of it is the
# operator's own business.

# ---------------------------------------------------------------------------
# Build the site
# ---------------------------------------------------------------------------
FROM node:22-bookworm-slim AS web-build
WORKDIR /src
COPY astro/package.json astro/package-lock.json ./astro/
RUN cd astro && npm ci
COPY astro ./astro
RUN cd astro && npx astro build

# ---------------------------------------------------------------------------
# Build the API
# ---------------------------------------------------------------------------
FROM golang:1.25-bookworm AS api-build
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# CGO off: the binary is static and needs nothing from the build image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/blogcms-server ./cmd/server

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
FROM node:22-bookworm-slim
WORKDIR /app

# The API binary and the built site. The site's `dist/` is the only thing that
# ships: content/ is read at request time and never baked in.
COPY --from=api-build /out/blogcms-server /app/blogcms-server
COPY --from=web-build /src/astro/dist /app/astro/dist
# The standalone server imports runtime packages (e.g. devalue) from
# node_modules; dist/ alone is not enough to start it.
COPY --from=web-build /src/astro/node_modules /app/astro/node_modules
COPY --from=web-build /src/astro/package.json /app/astro/package.json
COPY deploy/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# The three roots. They are the site's data, and they are mounted, not baked in.
RUN mkdir -p /app/content/posts /app/content/pages /app/content/system \
             /app/media /app/data

# The project's own presentation layer — the Markdown templates that carry the
# callout, card and figure styles — is *tracked*, so it travels with the image.
# The entry point copies it into an empty content volume, because the effect
# registry reads those files from CONTENT_ROOT at request time and a volume
# without them renders every article unstyled.
COPY content/system /app/pristine/system

VOLUME ["/app/content", "/app/media", "/app/data"]

ENV APP_DIR=/app \
    PORT=8080 \
    WEB_PORT=4321 \
    HOST=0.0.0.0 \
    CONTENT_ROOT=/app/content \
    MEDIA_ROOT=/app/media \
    DATA_ROOT=/app/data \
    API_BASE=http://127.0.0.1:8080

EXPOSE 4321
ENTRYPOINT ["/app/entrypoint.sh"]
