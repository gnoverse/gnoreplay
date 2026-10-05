FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /gnoreplay-server .

# Jobs build gno with the Go toolchain, so the runtime image keeps it.
FROM golang:1.26
RUN apt-get update && apt-get install -y --no-install-recommends git bubblewrap && rm -rf /var/lib/apt/lists/*
COPY --from=build /gnoreplay-server /usr/local/bin/gnoreplay-server
VOLUME /var/lib/gnoreplay
EXPOSE 8080
ENTRYPOINT ["gnoreplay-server", "--config", "/etc/gnoreplay/config.toml"]
