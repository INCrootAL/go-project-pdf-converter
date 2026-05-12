# PDF Converter Service

Асинхронный сервис конвертации документов в PDF с поддержкой LibreOffice и Chrome Headless. Создан для безопасной интеграции с Bitrix24.

## 🚀 Возможности

- **Конвертация офисных документов** (DOCX, XLSX, PPTX, ODT, RTF, TXT и др.) через LibreOffice
- **Конвертация HTML в PDF** через Chrome Headless с полной поддержкой CSS3 и шрифтов
- **Конвертация ZIP архивов** с HTML, CSS, шрифтами и изображениями
- **Асинхронная обработка** с ограничением параллельных конвертаций
- **Изолированная среда** - все операции в Docker контейнере, без доступа к основной системе
- **Потоковая передача** - файлы обрабатываются в памяти без сохранения на диск

## 📋 API Endpoints

### `POST /convert`
Конвертация офисных документов через LibreOffice

**Поддерживаемые форматы:**
DOC, DOCX, XLS, XLSX, PPT, PPTX, ODT, ODS, ODP, RTF, TXT, CSV, HTML

**Пример:**
```bash
curl -X POST http://localhost:5000/convert -F "file=@document.docx" -o result.pdf
```

### `POST /convert-html`
Конвертация HTML в PDF через Chrome Headless

**Пример:**
```bash
curl -X POST http://localhost:5000/convert-html \
  -F "html=<h1>Hello World</h1>" \
  -o result.pdf
POST /convert-zip
Конвертация ZIP архива с HTML, CSS, шрифтами и изображениями
```

### `POST /convert-zip`
Конвертация ZIP архива с HTML, CSS, шрифтами и изображениями

**Пример:**
```bash
curl -X POST http://localhost:5000/convert-zip \
  -F "file=@template.zip" \
  -o result.pdf
```

### `GET /health`
Проверка доступности сервиса

### `GET /stats`
Статистика конвертаций

## 🐳 Docker

### `Быстрый старт`

```bash
    git clone <repo-url>
    cd pdf-converter
    docker-compose up -d --build
```

### `Ограничение ресурсов`
В docker-compose.yml:

```yml
    deploy:
    resources:
        limits:
        cpus: '2'        # Максимум 2 ядра CPU
        memory: 1.5G     # Максимум 1.5 ГБ RAM
        reservations:
        cpus: '1'        # Минимум 1 ядро
        memory: 512M     # Минимум 512 МБ RAM
```

## 🔒 Безопасность

- Изолированный Docker контейнер без доступа к хост-системе
- Запуск от непривилегированного пользователя `appuser`
- Временные файлы создаются в `/tmp` и удаляются сразу после ответа
- Доступ только через внутреннюю сеть (`127.0.0.1:5000`)
- Ограничение CPU и памяти через Docker
- Нет доступа к файловой системе хоста
- Защита от DoS через семафор параллельных конвертаций