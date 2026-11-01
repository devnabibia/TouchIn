# go-sqlite3 uses cgo, so the build stage needs gcc (included in the golang image).
FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /inventory .

FROM debian:bookworm-slim
COPY --from=build /inventory /usr/local/bin/inventory
ENV DB_PATH=/data/inventory.db ADDR=:8080
VOLUME /data
EXPOSE 8080
CMD ["inventory"]
