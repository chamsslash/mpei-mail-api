FROM golang:1.27 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mpei-mail-api ./cmd/mpei-mail-api

FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/mpei-mail-api /mpei-mail-api

# В контейнере слушаем все интерфейсы: изоляцию обеспечивает сеть докера,
# а не биндинг. Дефолт самого бинаря остаётся 127.0.0.1.
ENV LISTEN_ADDR=0.0.0.0:8080
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/mpei-mail-api"]
