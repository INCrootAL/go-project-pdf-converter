# Используем официальный образ Go для сборки
FROM golang:1.21-bullseye AS builder

RUN apt-get update && apt-get install -y git
WORKDIR /app
COPY go.mod go.sum* ./
COPY main.go .
RUN if [ ! -f go.mod ]; then go mod init pdf-converter; fi
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o pdf-converter .

# Финальный образ
FROM debian:bullseye-slim

ENV DEBIAN_FRONTEND=noninteractive

# Устанавливаем всё одним RUN для уменьшения слоев
RUN apt-get update && apt-get install -y \
    curl \
    ca-certificates \
    wget \
    gnupg \
    fontconfig \
    fonts-dejavu \
    fonts-freefont-ttf \
    fonts-liberation \
    fonts-noto \
    openjdk-11-jre-headless \
    # Устанавливаем LibreOffice
    libreoffice \
    libreoffice-writer \
    libreoffice-calc \
    unzip \
    # Чистим кэш
    && rm -rf /var/lib/apt/lists/*

# Устанавливаем Google Chrome
RUN wget --no-check-certificate -q -O chrome.deb https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb \
    && apt-get update \
    && apt-get install -y ./chrome.deb \
    && rm chrome.deb \
    && rm -rf /var/lib/apt/lists/*

# Обновляем кэш шрифтов
RUN fc-cache -f -v

# Создаем пользователя
RUN groupadd -r appuser && useradd -r -g appuser -m appuser \
    && mkdir -p /home/appuser/.cache/dconf \
    && chown -R appuser:appuser /home/appuser

# Копируем приложение
COPY --from=builder --chown=appuser:appuser /app/pdf-converter /app/

USER appuser
WORKDIR /app
EXPOSE 5000
CMD ["./pdf-converter"]