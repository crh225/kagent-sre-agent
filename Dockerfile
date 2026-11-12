FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod .
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/flux-mcp ./cmd/flux-mcp

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/flux-mcp /flux-mcp
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/flux-mcp"]
