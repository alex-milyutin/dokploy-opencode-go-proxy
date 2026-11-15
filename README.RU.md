# dokploy-opencode-go-proxy

OpenAI-совместимый passthrough, который позволяет панели AI в Dokploy работать с
подпиской **OpenCode Go**, не покупая отдельные Models credits.

English version: [README.md](README.md)

## Зачем

Dokploy собирает провайдера через `@ai-sdk/openai-compatible` и жёстко проставляет
только `Authorization: Bearer …`
(`packages/server/src/utils/ai/select-ai-provider.ts`, ветка `custom`). Пользовательских
заголовков в `Settings → AI` нет: поля только `Name / API URL / API Key / Model`
(`apps/dokploy/server/api/routers/ai.ts`).

OpenCode Go, по документации («Where can I use it?»), требует на каждый не-GET запрос
стабильный `x-opencode-session`, иначе отдаёт `400 MissingSessionID`:

```
Request is missing x-opencode-session and cannot be routed efficiently.
```

Прокси добавляет этот заголовок (и свой `User-Agent`), а всё остальное передаёт как есть.

```
Dokploy AI panel ──► http://opencode-go-proxy:8080/v1 ──► https://opencode.ai/inference/go/openai/v1
   Authorization: Bearer <PROXY_TOKEN>       + x-opencode-session: ses_…
                                             + User-Agent: dokploy-ai/1.0
                                             Authorization: Bearer <OPENCODE_API_KEY>
```

## Что проксирует

| Метод | Путь | Поведение |
|---|---|---|
| `GET` | `/models` | прокидывается в `{UPSTREAM_BASE}/models` (сессия не нужна) |
| `POST` | `/chat/completions` | тело и статус как есть, добавляется `x-opencode-session`; стриминг (`stream: true`) проходит чанками |
| `GET` | `/healthz` | `ok` для healthcheck |

Ответ апстрима копируется дословно (код, тело, заголовки), поэтому сообщения об ошибках
вида «Insufficient balance» видны в Dokploy без искажений. В ответ добавляется
`X-Proxy-Session` — по нему видно, какой сессией ушёл запрос.

## Переменные окружения

| Переменная | Обязательна | По умолчанию | Смысл |
|---|---|---|---|
| `OPENCODE_API_KEY` | да | — | ключ OpenCode Go. Подставляется в запрос вместо клиентского |
| `PROXY_TOKEN` | сильно рекомендуем | — | что вводится в поле **API Key** в Dokploy; без него прокси открыт всем |
| `UPSTREAM_BASE` | нет | `https://opencode.ai/inference/go/openai/v1` | база апстрима |
| `LISTEN_ADDR` | нет | `:8080` | адрес прослушивания |
| `SESSION_MODE` | нет | `prefix` | `prefix` / `constant` / `request` (см. ниже) |
| `SESSION_ID` | нет | `ses_dokploy_proxy` | значение для `constant` и фолбэк |
| `USER_AGENT` | нет | `dokploy-ai/1.0` | свой UA, как просят в доке Go |

### Про сессию

Go хочет «стабильную на диалог» сессию, но у Dokploy диалогов нет — каждый вызов это
один `generateText` с единственным `prompt` (без истории). Поэтому «диалог» приближается
по инструкции:

- `prefix` (по умолчанию) — `ses_` + `sha256(model + первые 256 символов первого сообщения)`.
  Шаблон запроса у каждой фичи Dokploy свой (анализ логов build/runtime, генерация compose,
  варианты), а волатильная часть (сами логи) идёт дальше 256 символа → сессия стабильна
  внутри фичи и различается между фичами. Это то, что нужно кэшу промптов.
- `constant` — одна сессия на инстанс. Проще всего, весь трафик на один бэкенд.
- `request` — новый id на запрос. Не рекомендуется: теряется кэш-аффинити.

## Сборка и локальный запуск

```bash
GOTOOLCHAIN=go1.24.9 go build -o ocproxy .
OPENCODE_API_KEY=oc_sk_... PROXY_TOKEN=sk-local UPSTREAM_BASE=https://opencode.ai/inference/go/openai/v1 ./ocproxy

curl -s http://127.0.0.1:8080/healthz
curl -s -H 'Authorization: Bearer sk-local' http://127.0.0.1:8080/v1/models
```

Docker:

```bash
docker build -t dokploy-opencode-go-proxy .
docker run --rm -p 8080:8080 -e OPENCODE_API_KEY=oc_sk_... -e PROXY_TOKEN=sk-local dokploy-opencode-go-proxy
```

Нужен Go 1.24: бинарники, собранные более старым toolchain, на свежей macOS могут не
запускаться с `dyld: missing LC_UUID load command`.

## Деплой в Dokploy

Да, прокси поднимается в самом Dokploy — двумя способами.

### Вариант A: Compose-сервис (рекомендую)

`Create Service → Compose`, репозиторий с этим каталогом (или вставить `docker-compose.yml`
и `Dockerfile` руками). Dokploy собирает образ из `build: .`, запускает контейнер и
подключает его к сети `dokploy-network`, на которой живёт сама панель (установщик создаёт
`docker network create --driver overlay --attachable dokploy-network` и запускает панель в
ней). Порт наружу не публикуется: панель обращается к сервису по этой сети.

Переменные `OPENCODE_API_KEY` и `PROXY_TOKEN` задаются в `Environment` сервиса
(в Dokploy это шифруемые переменные). В `docker-compose.yml` используется
`${VAR:?set VAR}`, поэтому при отсутствии переменной деплой падает сразу, а не поднимает
прокси без авторизации.

### Вариант B: Application

`Create Service → Application` → провайдер Dockerfile, build path = корень этого каталога,
порт `8080`, домен не нужен. Плюс: Dokploy сам подхватит healthcheck из Dockerfile.

### Какой URL вписать в `Settings → AI`

Порядок проверки — от самого чистого к самому совместимому:

1. `http://opencode-go-proxy:8080/v1` — имя compose-сервиса в сети `dokploy-network`.
   Панель резолвит его по docker-DNS. Именно на это настроен поставляемый compose-файл:
   он вообще не публикует порт.
2. `http://<приватный IP узла>:8080/v1` — только если имя не резолвится. Тогда верни в
   compose блок `ports: ["8080:8080"]` или привяжи его к приватному интерфейсу.
   Приватные адреса в self-hosted Dokploy не блокируются: список приватных сетей в
   `provider-fetch.ts` применяется только под `IS_CLOUD` (метаданные облака режутся всегда).
3. `https://ai-proxy.<домен>/v1` через Traefik — только если нужен доступ снаружи хоста;
   тогда `PROXY_TOKEN` обязателен.

⚠️ Публикация `8080:8080` сама по себе не делает сервис приватным: порт отвечает по
хостовому имени узла, а Cloudflare проксирует порт 8080, поэтому прокси становится
доступен из интернета даже без отдельного домена. Токен защищает, но лучше имя сервиса
или firewall.

⚠️ `http://127.0.0.1:8080/v1` **не подойдёт**: панель живёт в контейнере, для неё 127.0.0.1 — это она сама.

Остальные поля:

| Поле | Значение |
|---|---|
| Name | `OpenCode Go (proxy)` |
| API URL | адрес из списка выше |
| API Key | значение `PROXY_TOKEN` (не ключ Go — он остаётся на сервере) |
| Model | `deepseek-v4.1-flash`, `kimi-k3`, `glm-5.3`, … (любая текстовая модель Go) |

Список моделей подтягивается из `/v1/models`, поэтому после сохранения в выпадающем списке
будут реальные ID Go. Протокол не важен: Console транслирует любой из четырёх API в любой,
поэтому модели с anthropic/responses-эндпоинтами тоже работают через `chat/completions`.

### Проверка после деплоя

```bash
curl -s http://opencode-go-proxy:8080/healthz          # ok
curl -s -H 'Authorization: Bearer <PROXY_TOKEN>' http://opencode-go-proxy:8080/v1/models
```

В Dokploy: `Settings → AI → Test connection`, затем любой анализ логов. В логах сервиса
прокси видно строку на каждый вызов:

```
POST /v1/chat/completions -> 200 in 640ms session="ses_78445081cec0a32b0ad919fd"
```

`400 MissingSessionID` в ответе означает одно из двух: запрос идёт мимо прокси (проверь
API URL) или прокси запущен со старым образом.

## Чего прокси не делает

- **Не транслирует протоколы** — это не нужно, Console делает это сам.
- **Не хранит историю** — Dokploy отправляет одиночный prompt, поэтому «сессия» это
  производная от инструкции, а не реальный диалог. Следствие: параллельные вызовы одной
  фичи делят один бэкенд и один KV-кэш. Для объёма запросов панели Dokploy это нормально;
  если начнут жаловаться на троттлинг, переключи `SESSION_MODE=request`.
- **Не заменяет** [`Dokploy#5576`](https://github.com/Dokploy/dokploy/issues/5576) /
  PR #5577 (custom headers на провайдера). Если PR смержат — прокси можно выключить,
  вписав Go-эндпоинт напрямую и задав `x-opencode-session` в UI.

## Тесты

```bash
python3 test/mock_upstream.py 9099 &
PROXY_BIN=./ocproxy scripts/smoke_test.sh
```

`test/mock_upstream.py` — фейковый OpenAI-совместимый апстрим, который записывает все
пришедшие запросы; `scripts/smoke_test.sh` проверяет: 401 без токена прокси, passthrough
`/v1/models`, стабильность сессии внутри фичи, различие между фичами, подмену ключа, свой
UA и SSE-стриминг. Реальный апстрим проверяется отдельно: с заведомо неверным ключом
приходит `401 authentication_error`, а не `MissingSessionID` — путь и заголовки доходят
корректно.
