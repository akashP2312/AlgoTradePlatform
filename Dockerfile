# One image holds every binary and the web UI; compose picks the command per service.
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 sh -c 'for c in ticker recorder jobs login backuprec sys gateway book riskmon botmon fakekite; do go build -trimpath -o /out/$c ./cmd/$c || exit 1; done'

# pg_dump and pg_restore must match the server's major version (17).
# Verify the alpine package name when bumping the base image.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata postgresql17-client \
 && adduser -D -u 10001 pmt \
 && mkdir -p /data && chown pmt:pmt /data
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY config/ /app/config/
COPY migrations/ /app/migrations/
COPY --from=web /web/dist/ /app/web/dist/
ENV TZ=Asia/Kolkata PMT_CONTAINER=1 PMT_CONFIG=/app/config/pmt.yaml PMT_DATA_DIR=/data
USER pmt
