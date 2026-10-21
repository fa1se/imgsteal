# Build a static binary.
FROM golang:1.27 AS build
WORKDIR /src
# Cache module downloads separately from the source.
COPY go.mod ./
RUN go mod download
COPY main.go ./
# Static, stripped binary so it runs on scratch.
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /imgsteal .

# Minimal runtime image: just the binary plus CA roots for upstream TLS.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /imgsteal /imgsteal
EXPOSE 8080
USER 65534:65534
ENTRYPOINT ["/imgsteal"]
