# Pure-Go build (modernc.org/sqlite needs no CGO), so a static binary is easy.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./cmd/server

FROM alpine:3.20
# git: vault push. yt-dlp: youtube transcript extraction (pulls python3).
RUN apk add --no-cache ca-certificates git yt-dlp
COPY --from=build /server /server
# Railway mounts a persistent volume at /data (DB_PATH + VAULT_PATH live here).
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/server"]
