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

## Dev-стенд с Invoker

Overlay `compose.dev-invoker.yaml` включает очередь Standard и открывает PostgreSQL, NATS и HTTPS MinIO только на loopback хоста. Не запускайте одновременно обычный dev-стенд на том же порту `8088`.

```sh
export PODTEST_DEV_DIR="$HOME/.config/podtest-dev"
systemctl --user start podman.socket
bash deploy/dev-invoker-config.sh
sh deploy/up.sh --dev -p podtest-dev -f compose.dev-invoker.yaml
```

На новом хосте установите `backend` и ReleaseSafe `podtest-invoker` в `$PODTEST_DEV_DIR/bin`. Invoker собирается с `-Dartifact-control=true -Dstandard-worker=true`; детали сборки и параметры `serve-standard` описаны в `docs/deployment.md` репозитория Invoker. Настройте два пользовательских unit: `podtest-dev-artifacts` запускает `backend invoker-artifacts`, а зависимый `podtest-dev-invoker` запускает `podtest-invoker serve-standard "$PODTEST_DEV_DIR/invoker.json"`. На текущем стенде оба unit установлены, а их `artifacts.env` и `invoker.env` находятся в `$PODTEST_DEV_DIR`.

Backend использует настройки PostgreSQL и S3 из `deploy/dev.env` с адресами `127.0.0.1:15432` и `https://127.0.0.1:19000`. Для artifact control задайте `INVOKER_ARTIFACTS_ADDR=127.0.0.1:17443`, `INVOKER_TLS_CERT` и `INVOKER_TLS_KEY` из `backend.crt` и `backend.key`, `INVOKER_CLIENT_CA` и `SSL_CERT_FILE` из `ca.crt`. Invoker использует тот же адрес control, `invoker.crt` и `invoker.key` для mTLS, `ca.crt` для control и MinIO, `nats://127.0.0.1:14222` и зафиксированные локальные ID образов для Shell и JavaScript.

```sh
curl -fsS http://localhost:8088/api/v1/health/ready
curl --cacert "$PODTEST_DEV_DIR/ca.crt" -fsS https://127.0.0.1:19000/minio/health/ready
systemctl --user start podtest-dev-invoker.service
python3 deploy/smoke-authoring.py --standard
```

Smoke-тест через Dex и HTTP API публикует Standard ZIP, отправляет shell-решение и проверяет `accepted`, общий балл `1.000000` и результат теста. Созданные записи остаются в базе. Если npm-реестр недоступен, `PODTEST_NPM_REGISTRY` позволяет указать доверенное зеркало для сборки без изменения lockfile.

Для C++-чекеров testlib задайте `PODTEST_TESTLIB_IMAGE_ID` в `invoker.env`: это ID локального доверенного образа GCC с `g++` в `/usr/local/bin/g++`. Для решений на C++23 и Python 3.14 задайте `PODTEST_STANDARD_CPP23_IMAGE_ID` (можно использовать тот же образ GCC) и `PODTEST_STANDARD_PYTHON314_IMAGE_ID` (образ с `/usr/local/bin/python3.14`). Проверка исходного архива «Сгенерируй графы!» на всех трёх языках:

```sh
python3 deploy/smoke-authoring.py --polygon-graphs "$HOME/Downloads/gen-graph-3\$linux.zip"
```

Для остановки с сохранением данных:

```sh
systemctl --user stop podtest-dev-invoker.service podtest-dev-artifacts.service
podman-compose -p podtest-dev --env-file deploy/dev.env -f compose.yaml -f compose.dev.yaml -f compose.dev-invoker.yaml down
```

## Запуск со своим OIDC

Скопируйте `.env.example` в `.env` и заполните пароль PostgreSQL, `DATABASE_URL`, ключи S3 и параметры OIDC. Не используйте локальное значение пароля `podtest-local-db` вне dev-среды. Укажите HTTPS origin в `PUBLIC_URL` и зарегистрируйте `${PUBLIC_URL}/auth/callback` в OIDC-провайдере. Для основного Compose настройте завершение TLS перед HTTP-портом Nginx и оставьте этот порт доступным только TLS-прокси. Запустите `sh deploy/up.sh`. Порты PostgreSQL, OpenFGA, NATS и MinIO остаются внутри сети Compose.

## Проверки

На странице контеста в **Problems** можно закрепить опубликованную ревизию существующей задачи (**Attach problem**) или создать и сразу добавить задачу из полного Polygon ZIP (**Import and attach**). Требуется `problem.xml` в корне; ZIP до 64 МиБ сохраняется целиком в приватном хранилище. Один набор готовых тестов с C++-чекером testlib, стандартным вводом и выводом, без групп и явных баллов конвертируется в исполняемый Standard-пакет с JavaScript, GNU C++23 (`g++ -std=c++23`) и Python 3.14. Ранее опубликованные ревизии не меняются: для новых языков архив нужно импортировать заново. Остальные архивы импортируются для диагностики с указанием причины. Секции условия отображаются как текст, без рендеринга LaTeX.

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

На dev-стенде с Invoker исполняются опубликованные пакеты Standard. Развёртывание dev-стенда не подтверждает готовность к production. Запуск Grader scripts не входит в этот режим.
