# Bases are pinned to the index digests `docker buildx imagetools inspect`
# read on 2026-10-16. The build uses the newest Go for its standard library
# fixes; go.mod asks only for go 1.24, which `go install` users may still have.
FROM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/track-record ./cmd/track-record && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/track-record /usr/local/bin/track-record
COPY --from=build --chown=65532:65532 /out/data /data
ENV TRACK_RECORD_DB=/data/track-record.db
USER 65532:65532
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/track-record"]
CMD ["serve"]
