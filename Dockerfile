# Fliable — single static binary, so the image is scratch-small.
#   docker build -t fliable .
#   docker run -p 8080:8080 -v fliable-data:/data fliable
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /fliable ./cmd/fliable

FROM scratch
COPY --from=build /fliable /fliable
# Journal storage lives here; mount a volume for durability.
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/fliable"]
CMD ["serve", "--addr", ":8080", "--data", "/data"]
