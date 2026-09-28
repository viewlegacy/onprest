FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
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
