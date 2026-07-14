# Pure-Go build (modernc.org/sqlite needs no CGO), so a static binary is easy.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./cmd/server

FROM alpine:3.20
# git: vault push. yt-dlp: youtube transcript extraction (pulls python3).
RUN apk add --no-cache ca-certificates git yt-dlp
COPY --from=build /server /server
# Persistence is a Railway Volume mounted at /data (DB_PATH + VAULT_PATH live
# there) — attach it on the service. Railway rejects the Docker VOLUME
# instruction, so it must NOT be declared here.
EXPOSE 8080
ENTRYPOINT ["/server"]
