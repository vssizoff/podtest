# Podtest

Система индивидуальных соревнований по программированию. Архитектура описана в `backend_frontend_architecture.md`, публичный REST API — в `openapi/openapi.yaml`.

## Локальный запуск

Нужны rootless Podman и `podman-compose`. Для удобства локального запуска Compose подставляет пароль PostgreSQL `podtest-local-db` и соответствующий `DATABASE_URL`, если переменные не заданы.

```sh
sh deploy/up.sh --dev
curl -fsS http://localhost:8088/api/v1/health/ready
```

Открывайте приложение по `http://localhost:8088`. Войдите через локальный Dex (`http://localhost:8088/dex`), используя `smoke@podtest.test` и пароль `podtest-local`. Dex доступен через Nginx: отдельный порт на хосте и запись в `/etc/hosts` не нужны. Тестовые credentials из `deploy/dev.env` и `deploy/dex.dev.yaml` предназначены для локальной разработки.

```sh
podman-compose --env-file deploy/dev.env -f compose.yaml -f compose.dev.yaml ps
podman-compose --env-file deploy/dev.env -f compose.yaml -f compose.dev.yaml logs backend-api backend-worker nginx
podman-compose --env-file deploy/dev.env -f compose.yaml -f compose.dev.yaml down
```

Скрипт запуска собирает образы, запускает инфраструктуру, выполняет одноразовые миграции PostgreSQL и OpenFGA, создаёт бакет и загружает модель авторизации. API и worker запускаются после успешного завершения этих команд. Поэтапный запуск учитывает поведение `podman-compose` 1.6, который передаёт завершившиеся контейнеры в `podman --requires`. Повторный запуск сохраняет данные в именованных томах.

Не заменяйте dev-скрипт на `podman-compose up -d --force-recreate`: эта команда не загружает `deploy/dev.env` и может не запустить зависимости после миграций. Если уже запускали основной Compose напрямую, выполните `podman-compose down` без `-v`, затем `sh deploy/up.sh --dev`. Если порт 8088 занят другим Compose-проектом, сначала остановите именно этот проект.

## Запуск со своим OIDC

Скопируйте `.env.example` в `.env` и заполните пароль PostgreSQL, `DATABASE_URL`, ключи S3 и параметры OIDC. Не используйте локальное значение пароля `podtest-local-db` вне dev-среды. Укажите HTTPS origin в `PUBLIC_URL` и зарегистрируйте `${PUBLIC_URL}/auth/callback` в OIDC-провайдере. Для основного Compose настройте завершение TLS перед HTTP-портом Nginx и оставьте этот порт доступным только TLS-прокси. Запустите `sh deploy/up.sh`. Порты PostgreSQL, OpenFGA, NATS и MinIO остаются внутри сети Compose.

## Проверки

На странице контеста в **Problems** можно закрепить опубликованную ревизию существующей задачи (**Attach problem**) или создать и сразу добавить задачу из полного Polygon ZIP (**Import and attach**). Требуется `problem.xml` в корне; ZIP до 64 МиБ сохраняется целиком в приватном хранилище. Исполняемый пакет и HTML-условие из Polygon пока не конвертируются.

Для пробной отправки выберите **Settings → Open**, разрешите самостоятельную регистрацию и сохраните настройки, затем нажмите **Join contest**. В dev-стеке `JUDGING_NO_INVOKER=true`: код сохраняется, а в истории появляется **Ошибка тестирования** (`testing_error`), без выполнения и начисления баллов. Обычный worker не отправляет неконвертированные пакеты Polygon в NATS.

В интерфейсе **Problems → New problem → New scripted Lua draft**. Сохраните условие в Markdown (сейчас интерфейс включает Python 3), загрузите ZIP до 16 МиБ с непустым `runner.lua` в корне и нажмите **Validate and publish**. Можно открыть существующую задачу и продолжить работу с черновиком. **Workspace → Inspect Polygon ZIP** только проверяет метаданные; задача из него пока не создаётся.

```sh
go test ./...
go vet ./...
go build ./...
npm --prefix frontend ci
npm --prefix frontend run build
protoc -I proto --descriptor_set_out=/tmp/podtest.pb proto/podtest/invoker/v1/control.proto
```

`python3 deploy/smoke-authoring.py` проверяет работу с задачами и курсами на запущенном dev-стеке. Нужен пакет Python `requests`; тест оставляет в базе созданные записи с префиксом `Authoring smoke`.

В текущем стеке backend сохраняет отправки и ставит попытки в очередь. Для выполнения решений потребуется новый Invoker. `control.proto` описывает предлагаемый контракт. Импорт Polygon пока возвращает диагностический отчёт. Сборка исполняемого пакета `standard` и запуск Grader scripts остаются незавершёнными.
