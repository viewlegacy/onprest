FROM golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG TARGET=gateway
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-X github.com/viewlegacy/onprest/internal/buildinfo.Version=${VERSION} -X github.com/viewlegacy/onprest/internal/buildinfo.ReleaseMarker=onprest-release-version:${VERSION}" -o /out/onprest ./cmd/${TARGET}

FROM gcr.io/distroless/base-debian12
COPY --from=build --chown=nonroot:nonroot /out/onprest /app/onprest
WORKDIR /app
USER nonroot:nonroot
ENTRYPOINT ["/app/onprest"]
