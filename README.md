# MCP Manager

Веб-панель управления MCP-серверами с поддержкой systemd.

## Возможности

- 🖥️ **Веб-интерфейс** на порту 9847 — список MCP-серверов, статус, логи, обнаруженные инструменты
- 🔄 **Real-time обновления** через WebSocket
- ⚙️ **Управление** — запуск, остановка, перезапуск серверов
- 📝 **Добавление серверов** — через форму или JSON прямо в интерфейсе
- ⚡ **Apply to CLI** — генерация конфигов для Claude, Codex, Gemini, Kilo, Antygravity, Open-Code
- 📦 **Экспорт/Импорт** — полный JSON конфигурации
- 🔧 **systemd** — работает как сервис

## Быстрый старт

```bash
# Собрать
make build

# Запустить локально
make run

# Открыть UI
open http://localhost:9847
```

## Установка как systemd-сервис

```bash
make install
sudo systemctl enable --now mcp-manager
```

## Конфигурация

Для systemd-сервиса конфиг хранится в `/etc/mcp-manager/config.json` в формате, совместимом с Claude Desktop:

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home"],
      "enabled": true
    },
    "brave-search": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-brave-search"],
      "env": {
        "BRAVE_API_KEY": "your-key"
      },
      "enabled": true
    }
  }
}
```

## API

| Endpoint | Method | Описание |
|---|---|---|
| `/api/servers` | GET | Список серверов со статусом |
| `/api/servers/{name}` | GET | Информация о сервере |
| `/api/servers/{name}` | PUT | Добавить/обновить сервер |
| `/api/servers/{name}` | DELETE | Удалить сервер |
| `/api/servers/{name}/start` | POST | Запустить сервер |
| `/api/servers/{name}/stop` | POST | Остановить сервер |
| `/api/servers/{name}/restart` | POST | Перезапустить сервер |
| `/api/config` | GET | Полный конфиг |
| `/api/config/export` | GET | Скачать конфиг как файл |
| `/api/config/import` | POST | Импортировать конфиг |
| `/api/apply/{tool}` | GET | Конфиг для CLI (claude/codex/gemini/kilo/antygravity/open-code) |
| `/ws` | WS | Real-time обновления |

## Как это работает

1. MCP Manager запускает MCP-серверы как дочерние процессы
2. Общается с ними по stdio используя MCP протокол (JSON-RPC)
3. Автоматически делает `initialize` + `tools/list` для обнаружения инструментов
4. Собирает stderr как логи
5. Отправляет обновления в UI через WebSocket

## Порт

По умолчанию: **9847** (можно изменить через `--port`)

## MCP Proxy Endpoint

Сервис теперь также работает как MCP-сервер (streamable HTTP) на endpoint:

- `POST/DELETE /mcp`

Пример подключения из `mcpServers`:

```json
{
  "mcpServers": {
    "mcp-catalog-proxy": {
      "type": "streamableHttp",
      "url": "http://127.0.0.1:9847/mcp"
    }
  }
}
```

Прокси агрегирует `tools/list` со всех `enabled` серверов и проксирует `tools/call`.
Имена инструментов публикуются как `serverName__toolName`.

Также проксируются:

- `prompts/list`, `prompts/get` (имена как `serverName__promptName`)
- `resources/list`, `resources/templates/list`, `resources/read` (URI переписываются в `mcp-catalog://...`)

## MCP Proxy over STDIO

Можно запускать этот сервис как локальный MCP server по stdio:

```bash
./mcp-manager --mcp-stdio
```

Пример подключения (stdio-клиенты):

```json
{
  "mcpServers": {
    "mcp-catalog-proxy-stdio": {
      "command": "/path/to/mcp-manager",
      "args": ["--mcp-stdio"]
    }
  }
}
```
