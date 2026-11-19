FROM golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS build
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
