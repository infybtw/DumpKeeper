FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/dumpkeeper ./cmd/dumpkeeper

FROM alpine:3.22
RUN apk add --no-cache postgresql-client ca-certificates tzdata
COPY --from=build /out/dumpkeeper /usr/local/bin/dumpkeeper
ENV DATA_DIR=/data LISTEN_ADDR=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK CMD wget -qO- http://127.0.0.1:8080/ >/dev/null || exit 1
ENTRYPOINT ["dumpkeeper"]
