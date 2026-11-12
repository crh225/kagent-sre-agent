FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /vision-mcp ./cmd/vision-mcp

FROM gcr.io/distroless/static:nonroot
COPY --from=build /vision-mcp /vision-mcp
EXPOSE 8080
ENTRYPOINT ["/vision-mcp"]
