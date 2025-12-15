# --- ビルドステージ ---
FROM golang:1.23-alpine AS builder

WORKDIR /app

# go.mod/sumを先にコピーしてキャッシュ活用
COPY go.mod go.sum ./
RUN go mod download

# ソースをコピーしてビルド
COPY . .
RUN go build -o bot-server main.go

# --- 実行ステージ ---
FROM alpine:latest

WORKDIR /app
# HTTPS通信用証明書
RUN apk --no-cache add ca-certificates

COPY --from=builder /app/bot-server .

EXPOSE 8080

CMD ["./bot-server"]