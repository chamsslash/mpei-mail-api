# alpine, а не полный golang: тот тянет 1.32 ГБ против 373 МБ, и на небольшом
# диске сервера сборка падает с «no space left on device». Бинарь всё равно
# статический (CGO_ENABLED=0), так что от базового образа ничего не наследует.
FROM golang:1.27-alpine AS build

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
