# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/tailterm-hub ./cmd/tailterm-hub \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/tt ./cmd/tt

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/tailterm-hub /tailterm-hub
COPY --from=build /out/tt /tt
ENV TAILTERM_STATE=/state TS_HOSTNAME=tailterm-hub
VOLUME ["/state"]
ENTRYPOINT ["/tailterm-hub"]
