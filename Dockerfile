FROM golang:1.25-alpine AS builder

WORKDIR /build
# Copy the whole context BEFORE resolving modules: go.mod has a filesystem `replace` pointing at
# third_party/rhizome-server-go (the in-tree private rhizome dep), so that path must exist before
# `go mod download`/build can resolve it. The persistent module + build caches keep rebuilds fast.
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -o /ultrabridge ./cmd/ultrabridge/ && \
    CGO_ENABLED=0 go build -o /ub-mcp ./cmd/ub-mcp/

FROM alpine:3.20 AS ub-mcp

RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /ub-mcp /usr/local/bin/ub-mcp

EXPOSE 8081
ENTRYPOINT ["ub-mcp"]
CMD ["--http", ":8081"]

FROM alpine:3.20 AS ultrabridge

RUN apk add --no-cache ca-certificates tzdata poppler-utils
COPY --from=builder /ultrabridge /usr/local/bin/ultrabridge

EXPOSE 8443
ENTRYPOINT ["ultrabridge"]
