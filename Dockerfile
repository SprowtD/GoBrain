# Pure-Go build (modernc.org/sqlite needs no CGO), so a static binary is easy.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./cmd/server

FROM alpine:3.20
# git + openssh-client: vault push over SSH (GIT_SSH_COMMAND shells out to `ssh`).
# yt-dlp from pip, not apk: the apk package lags and YouTube breaks stale
# extractors ("Only images are available for download"); pip tracks the latest
# release at build time. --break-system-packages: alpine's python is PEP 668
# externally-managed, and this is a single-purpose container.
# ffmpeg: extract + compress audio for the no-captions transcription fallback.
RUN apk add --no-cache ca-certificates git openssh-client python3 py3-pip ffmpeg \
 && pip install --no-cache-dir --break-system-packages -U yt-dlp
COPY --from=build /server /server
# Persistence is a Railway Volume mounted at /data (DB_PATH + VAULT_PATH live
# there) — attach it on the service. Railway rejects the Docker VOLUME
# instruction, so it must NOT be declared here.
EXPOSE 8080
ENTRYPOINT ["/server"]
