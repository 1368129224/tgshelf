# syntax=docker/dockerfile:1
# Multi-stage static build for tgshelf (Go).
#
# Runtime layout: data/ and downloads/ sit next to the binary (data/ and downloads/
# under the working directory /). The .parts temp directory stays inside the download
# directory (downloads/.parts); do not delete it before a download finishes. The image is
# distroless static, has no shell, and runs as the nonroot user.

FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/tgshelf ./cmd/tgshelf

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/tgshelf /tgshelf
WORKDIR /
ENV TZ=Asia/Shanghai
USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3   CMD ["/tgshelf", "-health"]

ENTRYPOINT ["/tgshelf"]
