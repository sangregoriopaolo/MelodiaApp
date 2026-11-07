FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/melodia-sync ./cmd/melodia-sync \
    && mkdir -p /out/data

FROM scratch
COPY --from=build /out/melodia-sync /melodia-sync
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV MELODIA_DB=/data/melodia.db \
    MELODIA_ADDR=:8080 \
    GOMEMLIMIT=32MiB
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/melodia-sync"]
CMD ["serve"]
