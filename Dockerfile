FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /hcagg .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hcagg /hcagg
EXPOSE 9050
ENTRYPOINT ["/hcagg"]
CMD ["-config", "/etc/hcagg/config.json"]
