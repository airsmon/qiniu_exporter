FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG REVISION=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
    -o /out/qiniu-exporter ./cmd/qiniu-exporter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/qiniu-exporter /qiniu-exporter
EXPOSE 9106
ENTRYPOINT ["/qiniu-exporter"]
CMD ["--config.file=/etc/qiniu-exporter/config.yaml"]
