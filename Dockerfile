FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/track-record ./cmd/track-record && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/track-record /usr/local/bin/track-record
COPY --from=build --chown=65532:65532 /out/data /data
ENV TRACK_RECORD_DB=/data/track-record.db
USER 65532:65532
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/track-record"]
CMD ["serve"]
