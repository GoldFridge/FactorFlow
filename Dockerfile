# The interface is built first: it is the slowest step to change and the cheapest to cache.
FROM node:22-alpine AS web
WORKDIR /src/web

COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS api
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO is off so the binary runs on a distroless image with no libc at all. The version is
# stamped in rather than guessed, because /healthz reports it and a demo has to be able to
# prove which build is live.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/factorflow ./cmd/factorflow

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=api /out/factorflow /app/factorflow
COPY --from=web /src/web/dist /app/web

ENV FF_HTTP_ADDR=:8080 \
    FF_WEB_DIR=/app/web

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/app/factorflow"]
