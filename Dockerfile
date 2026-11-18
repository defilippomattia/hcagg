FROM golang:1.27.4-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /hcagg .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hcagg /hcagg
EXPOSE 9050
ENTRYPOINT ["/hcagg"]
CMD ["-config", "/etc/hcagg/config.json"]
