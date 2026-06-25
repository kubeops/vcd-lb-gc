FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" \
    -o /out/vcd-lb-gc ./cmd/vcd-lb-gc

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/vcd-lb-gc /usr/local/bin/vcd-lb-gc
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/vcd-lb-gc"]
