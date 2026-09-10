# syntax=docker/dockerfile:1.7
ARG GO_IMAGE=__GO_IMAGE__
ARG RUNTIME_IMAGE=__RUNTIME_IMAGE__

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS builder
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY server/go.mod server/go.sum ./
# Keep modules in an exportable layer: cache mounts alone do not survive ephemeral CI builders.
RUN go mod download
COPY server/ ./
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.3.0
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -mod=readonly -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /out/latexmk-server ./cmd/server

FROM ${RUNTIME_IMAGE}
RUN test "$LATEXMK_IMAGE_PROFILE" = "__IMAGE_PROFILE__"
COPY --from=builder --chmod=0755 /out/latexmk-server /usr/local/bin/latexmk-server
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/latexmk-server"]
