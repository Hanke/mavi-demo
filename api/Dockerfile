FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
WORKDIR /app
COPY --from=build /out/api /app/api
# Migrations and seeds are read at runtime by `api migrate` / `api seed`.
COPY --from=infra . /app/infra/
EXPOSE 8080
ENTRYPOINT ["/app/api"]
