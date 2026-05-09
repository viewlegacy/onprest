FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG TARGET=gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/onprest ./cmd/${TARGET}

FROM gcr.io/distroless/base-debian12
COPY --from=build --chown=nonroot:nonroot /out/onprest /app/onprest
WORKDIR /app
USER nonroot:nonroot
ENTRYPOINT ["/app/onprest"]
