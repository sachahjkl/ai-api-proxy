FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /codex-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /codex-proxy /codex-proxy
EXPOSE 8080
ENTRYPOINT ["/codex-proxy"]
