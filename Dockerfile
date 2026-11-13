FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /vision-mcp ./cmd/vision-mcp
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /activity-ui ./cmd/activity-ui

FROM alpine:3.21 AS certificates
RUN apk add --no-cache ca-certificates

FROM gcr.io/distroless/static:nonroot
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /vision-mcp /vision-mcp
COPY --from=build /activity-ui /activity-ui
EXPOSE 8080
ENTRYPOINT ["/vision-mcp"]
