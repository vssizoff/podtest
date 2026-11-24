# Архитектура Backend и Frontend тестирующей системы

**Статус:** проектная спецификация, draft 1.0  
**Дата:** 23 ноября 2026 года  
**Область:** Backend, Frontend, PostgreSQL, OpenFGA, NATS, объектное хранилище и развёртывание через Podman Compose.  
**Не является:** полной спецификацией реализации Invoker / Execution Engine. Их контракты описываются здесь только в объёме интеграции с Backend.

---

## 1. Цели и принципы

Система предназначена для учебных курсов, контестов, задач по программированию, проверки решений и публикации оценок.

Основные принципы:

1. Backend — источник истины для пользователей, курсов, контестов, задач, ревизий, посылок, judging attempts и опубликованных результатов.
2. Frontend — Vue-приложение, которое обращается только к публичному HTTP API Backend. Оно не подключается напрямую к PostgreSQL, NATS, OpenFGA или Podman.
3. Backend реализуется как **модульный монолит на Go**. Логические Manager-ы имеют отдельные обязанности, но изначально не являются отдельными микросервисами.
4. Система использует **OIDC для пользовательской аутентификации** и **OpenFGA для авторизации на уровне ресурсов**.
5. Контесты являются индивидуальными: участник контеста — один пользователь. Командные контесты не входят в текущий объём.
6. Контесты могут объединяться в курсы. Один контест можно включить в несколько курсов.
7. Контест может содержать задачи и теоретические материалы.
8. Теоретические материалы поддерживают Markdown и LaTeX. В Markdown разрешены LaTeX-формулы.
9. Задачи имеют неизменяемые ревизии. Контест закрепляет конкретную ревизию задачи.
10. Для исполнения задач есть два формата: `standard` (Polygon) и `scripted_lua` (пользовательский `runner.lua`). HTTP, SQL и run-twice — готовые Lua-библиотеки/шаблоны, а не отдельные встроенные форматы.
11. Lua выставляет базовую оценку каждой посылки. Grader — отдельная система, которая при необходимости вычисляет итоговую оценку пользователя по множеству посылок этого или других участников.
12. Judging results и Grader results независимы и неизменяемы. Перепроверка создаёт новый judging attempt; пересчёт Grader создаёт новый grader run / score publication.
13. Большие файлы и логи хранятся в S3-compatible storage. PostgreSQL хранит метаданные и ссылки.
14. Сборка и развёртывание компонентов системы выполняются через `podman-compose`. Nginx — единая публичная точка входа: раздаёт Frontend static assets и проксирует запросы к Backend.

### 1.1. Принятые технологические решения

| Область | Технология / решение |
|---|---|
| Backend | Go |
| HTTP | `chi`, REST API |
| API specification | OpenAPI |
| Frontend | Vue + TypeScript + Vite |
| Основная БД | PostgreSQL |
| Очередь заданий | NATS JetStream |
| Backend ↔ Invoker | gRPC + Protocol Buffers |
| Файлы | S3-compatible storage, например MinIO |
| Авторизация ресурсов | OpenFGA |
| Пользовательская аутентификация | OAuth2/OIDC |
| Reverse proxy / static hosting | Nginx |
| Сборка и запуск контейнеров приложения | Podman + `podman-compose` |
| Исполнение решений | Отдельные Invoker-ы с rootless Podman |
| Grader | Отдельная подсистема; grader scripts на Python или Lua |

Рекомендуемые вспомогательные библиотеки Frontend (Vue Router, Pinia, генерация TypeScript API client из OpenAPI) являются предлагаемым способом организации приложения, а не отдельными уже подтверждёнными требованиями.

---

## 2. Развёртывание и сетевые границы

### 2.1. Публичная точка входа

Внешний трафик поступает только в Nginx.

```text
                       Browser
                          |
                       HTTPS
                          |
                    +-----v-----+
                    |   Nginx   |
                    |           |
                    | /        -> static frontend files
                    | /api/*   -> backend-api
                    | /auth/*  -> backend-api
                    | /events/*-> backend-api (SSE, если используется)
                    | /grpc/*  -> backend gRPC endpoint (если нужен внешний gRPC ingress)
                    +-----+-----+
                          |
                private Podman network
                          |
          +---------------+------------------+
          |               |                  |
    +-----v------+  +-----v------+    +------v------+
    | backend-api|  | PostgreSQL |    | OpenFGA     |
    +------------+  +------------+    +-------------+
          |
    +-----+---------------------------------------+
    |             Internal network                |
    |                                              |
    | NATS JetStream   MinIO   backend-worker      |
    | Grader workers   Invoker connectivity       |
    +----------------------------------------------+
```

Nginx должен:

- отдавать Vue SPA static files;
- использовать fallback на `index.html` для Frontend routes;
- проксировать `/api/` и `/auth/` в Backend;
- корректно проксировать SSE/долгие запросы, если выбран SSE для live-обновлений;
- при необходимости проксировать gRPC через `grpc_pass`, но не делать внутренние служебные методы публичными;
- задавать лимиты размера upload и timeout согласно требованиям загрузки посылок и файлов;
- передавать корректные `X-Forwarded-*` заголовки;
- не раскрывать внутренние адреса инфраструктуры.

Nginx — единственная точка входа для браузера. NATS, PostgreSQL, OpenFGA, MinIO API и Podman socket не публикуются наружу.

### 2.2. Backend processes

Рекомендуется собирать один Backend binary, который имеет разные режимы запуска:

- `backend api` — HTTP API, OIDC callback/session endpoints и gRPC endpoint для Invoker-ов (или отдельный listener в том же процессе);
- `backend worker` — Outbox publisher, NATS consumers, queue dispatch, обработка фоновых задач и запуск Grader jobs.

Это не два независимых продукта: они используют общий код, общие миграции и одну PostgreSQL schema. Их можно масштабировать отдельным количеством экземпляров.

Не следует запускать несколько конкурирующих миграторов при старте каждого контейнера. Миграции выполняются отдельной одноразовой командой/сервисом до readiness Backend.

### 2.3. Podman Compose

В корне репозитория должен находиться основной `compose.yaml` и compose override/profile для локальной разработки.

Минимальные сервисы основного окружения:

| Service | Роль | Публичный порт |
|---|---|---|
| `nginx` | Frontend static + reverse proxy | 80/443 |
| `backend-api` | REST/OIDC/gRPC ingress | Нет, только internal network |
| `backend-worker` | Фоновые процессы | Нет |
| `postgres` | PostgreSQL | Нет |
| `nats` | NATS JetStream | Нет |
| `minio` | S3 API | Нет; console только в dev при необходимости |
| `openfga` | Authorization service | Нет |
| `openfga-migrate` / model loader | Загрузка/обновление authorization model | Нет, one-shot |
| `db-migrate` | PostgreSQL migrations | Нет, one-shot |
| `grader-worker` | Исполнение Grader jobs | Нет |

Invoker-ы могут находиться на отдельных execution hosts. Для них допускается отдельный compose stack из тех же образов/версий. Если Invoker запускается контейнером на том же хосте, он получает доступ к rootless Podman socket через явный bind mount. Это требует проверки UID/GID, прав socket и конфигурации user namespace; нельзя автоматически монтировать весь `/run/user`.

Все application images должны собираться через compose build из исходного репозитория. Для Nginx допустим multistage Dockerfile: Node/Vite stage собирает Frontend, итоговый Nginx image получает только `dist/` и nginx config.

Основной способ запуска:

```bash
podman-compose build
podman-compose up -d
```

Команды миграций, проверки статуса и остановки должны быть описаны в README. Compose-файл не должен требовать Docker daemon.

### 2.4. Сетевые сегменты

Рекомендуется разделить сети:

- `edge` — Nginx ↔ backend-api;
- `app` — Backend ↔ PostgreSQL/NATS/MinIO/OpenFGA;
- `execution-control` — связь Backend и удалённых Invoker-ов/Grader workers;
- `grader-isolation` — среда Grader Worker, не имеющая прямого доступа к PostgreSQL.

Не обязательно создавать все сети как отдельные физические bridge network при первой итерации, но правила доступа должны быть отражены в compose и firewall.

---

## 3. Логическая архитектура Backend

Manager-ы — внутренние Go packages/services. Они не должны быть отдельными сетевыми микросервисами на старте.

### 3.1. AuthenticationManager

Отвечает за OAuth2/OIDC flow и пользовательские сессии:

- Authorization Code flow;
- PKCE (`S256`);
- проверка `state` и `nonce`;
- проверка подписи, issuer, audience, expiration и других обязательных claims;
- callback;
- logout и отзыв локальной сессии;
- обновление токенов, если выбран refresh-token flow;
- безопасная работа за reverse proxy.

Рекомендуемый браузерный паттерн — BFF: Backend хранит OAuth tokens на серверной стороне, Frontend получает защищённую session cookie (`Secure`, `HttpOnly`, подходящий `SameSite`). Не хранить refresh/access token в `localStorage`.

Пользователь идентифицируется по паре `issuer + subject`, а не только по email.

### 3.2. IdentityManager

- создаёт локального пользователя при первом успешном входе;
- сопоставляет внешние identity с внутренним `user_id`;
- хранит отображаемое имя, email, avatar URL и статус;
- обновляет профиль по доверенным OIDC claims;
- поддерживает привязку дополнительных identity только отдельным подтверждённым процессом.

### 3.3. AuthorizationManager

Единый адаптер к OpenFGA:

- `Check(user, relation, object)`;
- batch checks;
- выдача и отзыв relationships;
- загрузка версионированной authorization model;
- синхронизация отношений;
- контролируемая обработка ошибок OpenFGA.

OpenFGA решает, имеет ли пользователь право на действие с конкретным объектом. Backend дополнительно применяет бизнес-ограничения: временные границы контеста, открытость отправки, лимиты посылок, состояние ревизии и видимость результата.

Предлагаемая модель отношений:

- Course: owner, manager, teacher, student, viewer;
- Contest: manager, participant, viewer; связь с Course может наследовать некоторые права;
- Problem: owner, editor, viewer;
- ContestProblem: связь с Contest и Problem;
- Material: доступ наследуется от Contest;
- Submission: owner и доступ менеджеров соответствующего Contest.

Это логическая модель. Перед выпуском нужно оформить и протестировать точную OpenFGA DSL model.

Если несколько приложений используют один OpenFGA deployment, необходимо определить Store strategy. Один общий Store подходит только при согласованной общей модели и совместном графе отношений. Если доменные модели независимы, можно иметь отдельные Stores в одном OpenFGA deployment.

Рекомендуемая консистентность: PostgreSQL хранит доменные членства/назначения ролей как источник фактов, а изменения отношений публикуются через `authorization_outbox` и синхронизируются с OpenFGA. При временном рассогласовании выдача новых прав должна быть fail-closed; отзыв прав должен блокироваться локальной политикой немедленно, не дожидаясь синхронизации tuple.

### 3.4. CourseManager

- создание, изменение, архивирование курса;
- назначение владельца и менеджеров;
- добавление/удаление контестов;
- порядок контестов в курсе;
- получение списка курсов и страницы курса.

Курс содержит контесты, но не владеет их задачами напрямую.

### 3.5. ContestManager

- CRUD контестов;
- расписание начала и окончания;
- статус контеста;
- добавление задач;
- выбор закреплённой ревизии каждой задачи;
- настройки участника и регистрации;
- queue/submission limits;
- видимость вердиктов по каждой задаче;
- настройки scoreboard;
- роли менеджеров/участников.

Контест индивидуальный. `ContestParticipant` соответствует одному `User` в одном `Contest`.

Связь Course ↔ Contest рекомендуется many-to-many: один контест можно включить в несколько курсов.

### 3.6. ProblemManager

Отвечает за задачи и ревизии:

- создание задачи;
- импорт Polygon package/API (поддерживаемые способы импорта должны быть описаны отдельной import compatibility matrix);
- проверка целостности импортированного пакета;
- создание неизменяемой revision;
- хранение manifest и всех файлов задачи;
- формирование execution bundle;
- предоставление Invoker-у точного архива по revision ID и digest;
- переключение ContestProblem на новую revision только явным действием менеджера;
- предупреждение менеджеру, когда автор опубликовал новую revision уже используемой задачи.

При изменении любого компонента задачи создаётся новая revision целиком. Отдельное версионирование checker, tests, runner и grader не вводится.

Встроенные execution formats:

- `standard` — Polygon;
- `scripted_lua` — `runner.lua`.

HTTP, SQL, run-twice и другие сценарии реализуются Lua-библиотеками/шаблонами.

### 3.7. StatementManager

Управляет statement, локализациями, изображениями и ресурсами условия.

- statement является частью ProblemRevision;
- поддерживаются локали;
- исходный текст хранится отдельно от HTML;
- изменение statement создаёт новую ProblemRevision;
- рендеринг/санитизация/кэширование выполняются через DocumentRenderer;
- frontend получает подготовленный безопасный HTML и необходимые CSS/static assets.

### 3.8. MaterialManager

Теоретические материалы прикрепляются к Contest отдельно от задач.

Поддерживаемые исходные форматы:

1. `markdown` — Markdown с поддержкой inline и display LaTeX;
2. `latex` — самостоятельный LaTeX source.

Обязанности:

- CRUD материалов;
- независимые revisions материалов;
- привязка конкретной MaterialRevision к Contest;
- сортировка материалов в Contest;
- вложения и изображения;
- рендеринг исходного текста в HTML;
- хранение digest и версии renderer;
- управление видимостью материала в контесте.

Изменение теоретического материала **не создаёт новую revision задачи**.

### 3.9. DocumentRenderer

Общий компонент для StatementManager и MaterialManager:

- Markdown → HTML;
- Markdown math extension: `$...$`, `$$...$$`, `\\(...\\)`, `\\[...\\]` согласно принятому синтаксису;
- LaTeX → HTML/MathML/рендеренные формулы;
- обработка изображений и локальных ресурсов;
- санитизация HTML;
- запрет произвольного JavaScript и небезопасных ссылок;
- cache по source digest + renderer version + settings.

LaTeX-компиляция производится на Backend стороне. Frontend получает HTML и стандартные CSS/JS ресурсы выбранного рендерера (KaTeX или MathJax). Необходимо выбрать одну основную реализацию для одинакового поведения и зафиксировать её версию. Нельзя принимать пользовательский TeX как произвольную команду shell.

### 3.10. SubmissionManager

- приём посылки;
- проверка прав и состояния Contest;
- проверка contest submission limit;
- сохранение source artifact;
- создание Submission;
- запрос создания JudgingAttempt;
- история посылок пользователя;
- выдача source/result только при соответствующих правах.

Исходная Submission неизменяема. Её judging attempts могут быть множественными.

### 3.11. JudgingManager

- создание judging attempt;
- привязка attempt к Submission, ProblemRevision и execution bundle digest;
- запрос постановки в очередь;
- отмена attempt;
- ручная перепроверка;
- перевод состояния;
- контроль завершения и ошибок;
- запуск Grader после появления новой базовой оценки, если он настроен.

Перепроверка — новый attempt с новым ID. Старый результат остаётся неизменным. Attempt должен ссылаться на конкретную ProblemRevision, включая случай перепроверки после явного переключения ContestProblem на новую revision.

### 3.12. QueueManager

- хранит очередь ожидания в PostgreSQL;
- публикует runnable jobs в NATS JetStream через Outbox;
- контролирует ограничения Contest;
- выбирает доступные Invoker-ы совместно с InvokerManager;
- обновляет lease/assignment;
- не реализует приоритеты (приоритеты не предусмотрены).

Принятые queue/submission constraints:

- в Contest задаётся максимальное количество посылок в очереди;
- если лимит превышен, новая посылка остаётся в ожидании допуска, пока у пользователя не протестируется хотя бы одна посылка;
- задаётся максимальное количество посылок на задачу;
- приоритетов нет.

Точное определение queue cap (per-user/per-contest), окно подсчёта и трактовка «посылка протестировалась» требуют отдельной фиксации.

### 3.13. InvokerManager

- реестр Invoker-ов;
- heartbeat и offline detection;
- capabilities и версия протокола;
- свободные/занятые слоты;
- назначение задания;
- lease и восстановление;
- ограничение назначения несовместимому Invoker;
- учёт нескольких Invoker на разных машинах.

Все Invoker-ы поддерживают оба execution format. Архитектура допускает несколько экземпляров, распределяемых Backend-ом.

### 3.14. JudgingEventManager

Принимает gRPC события Invoker:

- проверяет `run_id`, `submission_id`, `judging_attempt_id`;
- дедуплицирует по `event_id`;
- проверяет sequence number;
- сохраняет состояние;
- обрабатывает BuildLog, TestStarted, TestFinished, ProblemScore, ArtifactCreated, RunnerError, RunTimeout, RunFinished;
- не изменяет уже завершённый attempt по запоздавшим событиям.

Большие файлы загружаются в S3 и передаются как artifact references. gRPC не используется для бесконечных строк/огромных архивов.

### 3.15. GraderManager

Grader не проверяет отдельное решение вместо Invoker и не изменяет его judging results. Он рассчитывает итоговую оценку пользователя по множеству посылок.

- базовый score каждой Submission устанавливает Lua или Standard Executor;
- без Grader используется стандартная политика (по умолчанию — лучший base score пользователя по задаче);
- при включённом Grader создаётся snapshot допустимых Submission/JudgingAttempt IDs;
- Grader запрашивает данные через Backend API, прямого доступа к PostgreSQL не имеет;
- grader scripts поддерживаются на Python или Lua;
- Grader может учитывать собственные и чужие посылки согласно настройкам доступа;
- режимы запуска: автоматический при новой посылке и ручной запуск менеджером;
- режим по расписанию не предусматривается;
- preview не меняет опубликованные оценки;
- публикация нового набора оценок атомарна.

Grader должен возвращать оценку пользователя по ContestProblem. Базовые оценки Submission остаются неизменными.

### 3.16. ScoreboardManager

- формирует participant × contest problem таблицу;
- без Grader использует best base score;
- с Grader использует последнюю активную опубликованную ScorePublication;
- учитывает scoreboard settings;
- готовит публичное и менеджерское представление;
- не пересчитывает judging results.

### 3.17. ArtifactManager

Файлы хранятся в S3-compatible storage. PostgreSQL хранит metadata, digest, size, media type и storage key.

Виды артефактов:

- исходный пакет Polygon;
- revision bundle;
- исходный код/архив Submission;
- build logs;
- runner logs;
- test stdin/stdout/stderr;
- checker logs;
- дополнительные файлы задачи;
- grader logs/outputs;
- вложения Statement/Material.

Доступ к файлам должен проходить через Backend authorization. Допустимо выдавать короткоживущие presigned URLs, если права проверены до их выдачи.

### 3.18. Дополнительные модули

- `LanguageManager` — языки, шаблоны, build/run commands и custom Containerfile definitions;
- `AuditManager` — история действий авторов/менеджеров;
- `NotificationManager` — уведомления о статусах/публикации результатов;
- `SettingsManager` — системные default policies.

Они не обязаны быть отдельными сервисами.

---

## 4. Логическая схема данных PostgreSQL

Все UUID — `uuid`. Все даты — `timestamptz` в UTC. Для денежных/балльных значений использовать `numeric`, а не float/double, чтобы избежать ошибок округления в БД. API может принимать JSON number, но Backend проверяет диапазон и преобразует в decimal representation.

### 4.1. Users / OIDC

#### `users`

- `id UUID PK`
- `display_name TEXT`
- `email TEXT NULL`
- `avatar_url TEXT NULL`
- `status TEXT NOT NULL` — active/disabled/pending;
- `created_at`, `updated_at TIMESTAMPTZ NOT NULL`.

#### `user_identities`

- `id UUID PK`
- `user_id UUID FK users`
- `issuer TEXT NOT NULL`
- `subject TEXT NOT NULL`
- `created_at TIMESTAMPTZ NOT NULL`
- `UNIQUE(issuer, subject)`.

Email не является внешним идентификатором пользователя. Внешняя идентичность задаётся парой issuer + subject.

#### `auth_sessions` (если выбран server-side BFF session)

- `id UUID PK`
- `session_token_hash TEXT UNIQUE NOT NULL`
- `user_id UUID FK users`
- `created_at`, `expires_at`, `last_seen_at`
- `revoked_at TIMESTAMPTZ NULL`
- минимальные metadata (например, user-agent digest / IP policy), без лишних персональных данных.

OAuth access/refresh tokens не должны попадать в browser storage. Секреты session token хранятся только в виде хеша или в защищённом серверном secret store.

### 4.2. Courses / Contests

#### `courses`

- `id UUID PK`
- `slug TEXT UNIQUE NOT NULL`
- `title TEXT NOT NULL`
- `description TEXT NULL`
- `created_by UUID FK users`
- `created_at`, `updated_at`
- `archived_at TIMESTAMPTZ NULL`.

#### `contests`

- `id UUID PK`
- `slug TEXT UNIQUE NOT NULL`
- `title TEXT NOT NULL`
- `description TEXT NULL`
- `status TEXT NOT NULL`
- `created_by UUID FK users`
- `starts_at`, `ends_at TIMESTAMPTZ NULL`
- `current_score_publication_id UUID NULL` (FK может быть добавлен отдельной миграцией из-за взаимной ссылки)
- `created_at`, `updated_at`, `archived_at`.

#### `course_contests`

- `course_id UUID FK courses`
- `contest_id UUID FK contests`
- `position INTEGER NOT NULL`
- `added_at TIMESTAMPTZ NOT NULL`
- `PRIMARY KEY(course_id, contest_id)`
- индекс `(contest_id)`.

Модель many-to-many принята как гибкая исходная структура. Если бизнес-правило позднее запретит совместное использование контеста в нескольких курсах, добавляется `UNIQUE(contest_id)`.

#### `contest_settings`

- `contest_id UUID PK/FK contests`
- `max_queued_submissions INTEGER NULL`
- `max_submissions_per_problem INTEGER NULL`
- `allow_submissions BOOLEAN NOT NULL`
- `scoreboard_visibility TEXT NOT NULL`
- `settings JSONB NOT NULL DEFAULT '{}'`
- `updated_at TIMESTAMPTZ NOT NULL`.

В JSONB допустимы редко используемые расширения. Настройки, влияющие на ограничения и частые запросы, должны быть типизированными колонками.

#### `contest_participants`

- `id UUID PK`
- `contest_id UUID FK contests`
- `user_id UUID FK users`
- `status TEXT NOT NULL`
- `registered_at TIMESTAMPTZ NOT NULL`
- `UNIQUE(contest_id, user_id)`.

**Только индивидуальное участие.** Один participant соответствует одному пользователю. Таблицы teams/team_members и командные посылки не создаются.

#### `contest_memberships` / domain roles

Если роли и membership facts хранятся в PostgreSQL как источник истины для синхронизации с OpenFGA:

- `contest_id`, `user_id`, `role`, `created_at`;
- unique `(contest_id, user_id, role)`.

Аналогичные memberships для Course. Нельзя одновременно считать membership canonical и независимо вручную изменять ту же связь только в OpenFGA.

### 4.3. Problems / revisions

#### `problems`

- `id UUID PK`
- `slug TEXT UNIQUE NOT NULL`
- `title TEXT NOT NULL`
- `created_by UUID FK users`
- `current_revision_id UUID NULL`
- `created_at`, `updated_at`, `archived_at`.

#### `problem_revisions`

- `id UUID PK`
- `problem_id UUID FK problems`
- `revision_number INTEGER NOT NULL`
- `execution_format TEXT NOT NULL CHECK IN ('standard','scripted_lua')`
- `execution_format_version INTEGER NOT NULL`
- `runner_api_version TEXT NULL`
- `manifest JSONB NOT NULL`
- `source_artifact_id UUID FK artifacts NULL`
- `bundle_artifact_id UUID FK artifacts NULL`
- `content_digest TEXT NOT NULL`
- `status TEXT NOT NULL` — draft/validated/published/retired
- `created_by UUID FK users`
- `created_at TIMESTAMPTZ NOT NULL`
- `UNIQUE(problem_id, revision_number)`.

Revision целиком неизменяема после публикации. Изменение tests/checker/runner/statement/language config/grader config создаёт новую revision.

#### `problem_statements`

- `id UUID PK`
- `problem_revision_id UUID FK problem_revisions`
- `locale TEXT NOT NULL`
- `source_format TEXT NOT NULL`
- `source_text TEXT NOT NULL`
- `source_digest TEXT NOT NULL`
- `rendered_artifact_id UUID FK artifacts NULL`
- `renderer_version TEXT NULL`
- `render_status TEXT NOT NULL`
- `UNIQUE(problem_revision_id, locale)`.

Statement относится к revision задачи. Его изменение требует новой ProblemRevision.

#### `contest_problems`

- `id UUID PK`
- `contest_id UUID FK contests`
- `problem_id UUID FK problems`
- `problem_revision_id UUID FK problem_revisions`
- `position INTEGER NOT NULL`
- `max_score NUMERIC(20,8) NULL`
- `submission_limit INTEGER NULL`
- `verdict_visibility JSONB NOT NULL DEFAULT '{}'`
- `created_at TIMESTAMPTZ NOT NULL`
- `UNIQUE(contest_id, problem_id)` или иное явное правило повторного добавления одной задачи.
- `UNIQUE(contest_id, position)`.

`contest_problems.problem_revision_id` фиксирует используемую контестом revision. Новая revision не подменяет текущую автоматически.

### 4.4. Languages

#### `languages`

- `id UUID PK`
- `slug TEXT UNIQUE NOT NULL`
- `name TEXT NOT NULL`
- `is_builtin BOOLEAN NOT NULL`
- `status TEXT NOT NULL`
- `default_template_id UUID NULL`
- `created_at`, `updated_at`.

#### `problem_revision_languages`

- `problem_revision_id UUID FK problem_revisions`
- `language_id UUID FK languages`
- `enabled BOOLEAN NOT NULL`
- `build_config JSONB NOT NULL`
- `run_config JSONB NOT NULL`
- `containerfile_artifact_id UUID FK artifacts NULL`
- `PRIMARY KEY(problem_revision_id, language_id)`.

Для Polygon задач стандартные языки доступны по умолчанию. Автор может добавить собственный язык, выбрать шаблон или собственный Containerfile и задать команды сборки/запуска. Build/runtime конфигурация должна проходить валидацию и применяться в пределах политик Invoker.

### 4.5. Theory materials

#### `theory_materials`

- `id UUID PK`
- `title TEXT NOT NULL`
- `created_by UUID FK users`
- `created_at TIMESTAMPTZ NOT NULL`
- `archived_at TIMESTAMPTZ NULL`.

#### `material_revisions`

- `id UUID PK`
- `material_id UUID FK theory_materials`
- `revision_number INTEGER NOT NULL`
- `format TEXT NOT NULL CHECK IN ('markdown','latex')`
- `source_text TEXT NOT NULL`
- `source_digest TEXT NOT NULL`
- `rendered_artifact_id UUID FK artifacts NULL`
- `renderer_version TEXT NULL`
- `render_status TEXT NOT NULL`
- `created_at TIMESTAMPTZ NOT NULL`
- `UNIQUE(material_id, revision_number)`.

#### `contest_materials`

- `id UUID PK`
- `contest_id UUID FK contests`
- `material_revision_id UUID FK material_revisions`
- `position INTEGER NOT NULL`
- `visibility TEXT NOT NULL`
- `added_at TIMESTAMPTZ NOT NULL`
- `UNIQUE(contest_id, position)`.

Материал имеет независимую историю ревизий. Контест закрепляет конкретную MaterialRevision. Изменение теории не создаёт ProblemRevision.

### 4.6. Submissions / judging

#### `submissions`

- `id UUID PK`
- `contest_problem_id UUID FK contest_problems`
- `participant_id UUID FK contest_participants`
- `submitted_by UUID FK users`
- `problem_revision_id UUID FK problem_revisions` — revision на момент отправки
- `language_id UUID FK languages NULL`
- `source_artifact_id UUID FK artifacts`
- `source_digest TEXT NOT NULL`
- `submitted_at TIMESTAMPTZ NOT NULL`.

Submission — неизменяемая запись отправки. Исходный архив/код сохраняется в S3.

#### `judging_attempts`

- `id UUID PK`
- `submission_id UUID FK submissions`
- `attempt_number INTEGER NOT NULL`
- `problem_revision_id UUID FK problem_revisions`
- `status TEXT NOT NULL`
- `verdict TEXT NULL`
- `base_score NUMERIC(20,8) NULL`
- `triggered_by UUID FK users NULL`
- `trigger_reason TEXT NULL`
- `retry_of_attempt_id UUID FK judging_attempts NULL`
- `invoker_id UUID FK invokers NULL`
- `queued_at`, `started_at`, `finished_at`, `created_at TIMESTAMPTZ`
- `UNIQUE(submission_id, attempt_number)`.

Первоначальный attempt обычно ссылается на revision из Submission. При явной перепроверке менеджер может создать новый attempt на другой revision, сохраняя старый attempt.

Рекомендуемые состояния:

`created → waiting → queued → assigned → running → completed | failed | cancelled`.

#### `test_results`

- `id UUID PK`
- `attempt_id UUID FK judging_attempts`
- `test_id TEXT NOT NULL`
- `verdict TEXT NOT NULL`
- `score NUMERIC(20,8) NULL`
- `checker_metadata TEXT NULL`
- `created_at TIMESTAMPTZ NOT NULL`
- `UNIQUE(attempt_id, test_id)`.

`checker_metadata` — отдельное поле, не смешивается с verdict/score/logs. Конкретный лимит размера и кодировка metadata устанавливаются контрактом API.

#### `subgroup_results`

- `id UUID PK`
- `attempt_id UUID FK judging_attempts`
- `subgroup_id TEXT NOT NULL`
- `score NUMERIC(20,8) NOT NULL`
- `grader_metadata TEXT NULL` (если этот payload остаётся частью контракта)
- `UNIQUE(attempt_id, subgroup_id)`.

### 4.7. Queue / Invokers / events

#### `queue_entries`

- `id UUID PK`
- `judging_attempt_id UUID UNIQUE FK judging_attempts`
- `contest_id UUID FK contests`
- `participant_id UUID FK contest_participants`
- `status TEXT NOT NULL`
- `created_at`, `queued_at`, `assigned_at`, `completed_at TIMESTAMPTZ NULL`.

Queue entries нужны для ожидания допуска, queue limits, отображения места/статуса и восстановления после сбоев. NATS JetStream отвечает за доставку задания после допуска.

#### `invokers`

- `id UUID PK`
- `instance_id TEXT UNIQUE NOT NULL`
- `address TEXT NOT NULL`
- `status TEXT NOT NULL`
- `last_heartbeat_at TIMESTAMPTZ NULL`
- `capacity JSONB NOT NULL`
- `capabilities JSONB NOT NULL`
- `created_at`, `updated_at TIMESTAMPTZ NOT NULL`.

Capabilities должны включать protocol/API versions, x86-64 architecture, версии runtime и поддерживаемые execution formats.

#### `judging_events`

- `id UUID PK`
- `attempt_id UUID FK judging_attempts`
- `event_id UUID NOT NULL`
- `sequence_number BIGINT NOT NULL`
- `event_type TEXT NOT NULL`
- `payload JSONB NOT NULL`
- `received_at TIMESTAMPTZ NOT NULL`
- `UNIQUE(attempt_id, event_id)`
- `UNIQUE(attempt_id, sequence_number)`.

Events используются для дедупликации, аудита и восстановления хода проверки. Итоговые данные дополнительно нормализуются в judging tables.

#### `outbox_events`

- `id UUID PK`
- `event_type TEXT NOT NULL`
- `aggregate_id UUID NOT NULL`
- `payload JSONB NOT NULL`
- `created_at TIMESTAMPTZ NOT NULL`
- `published_at TIMESTAMPTZ NULL`
- `attempts INTEGER NOT NULL DEFAULT 0`.

Запись Submission/JudgingAttempt/QueueEntry и соответствующий Outbox event выполняются в одной PostgreSQL transaction.

#### `inbox_events` (или эквивалентная таблица дедупликации)

- `consumer_name TEXT NOT NULL`
- `event_id UUID NOT NULL`
- `processed_at TIMESTAMPTZ NOT NULL`
- `PRIMARY KEY(consumer_name, event_id)`.

### 4.8. Artifacts

#### `artifacts`

- `id UUID PK`
- `storage_key TEXT UNIQUE NOT NULL`
- `artifact_type TEXT NOT NULL`
- `media_type TEXT NULL`
- `size_bytes BIGINT NOT NULL`
- `digest TEXT NOT NULL`
- `created_at TIMESTAMPTZ NOT NULL`.

#### `attempt_artifacts`

- `attempt_id UUID FK judging_attempts`
- `artifact_id UUID FK artifacts`
- `kind TEXT NOT NULL`
- `test_id TEXT NULL`
- `created_at TIMESTAMPTZ NOT NULL`
- composite primary/unique key according to artifact cardinality.

Большие логи и stdin/stdout/stderr не нужно держать целиком в PostgreSQL или отправлять одним gRPC-сообщением. Invoker загружает артефакт в object storage, затем отправляет его metadata/reference.

### 4.9. Grader / score publication

#### `grader_runs`

- `id UUID PK`
- `contest_id UUID FK contests`
- `contest_problem_id UUID FK contest_problems NULL` (если grader обрабатывает несколько задач/весь контест)
- `problem_revision_id UUID FK problem_revisions`
- `trigger_type TEXT NOT NULL` — `automatic_submission` или `manual`
- `run_mode TEXT NOT NULL` — `preview` или `publish`
- `status TEXT NOT NULL`
- `snapshot_id UUID NOT NULL`
- `snapshot_digest TEXT NOT NULL`
- `triggered_by UUID FK users NULL`
- `started_at`, `finished_at`, `created_at TIMESTAMPTZ`.

Расписание запуска Grader не предусматривается. Допустимы автоматический запуск при новой посылке и ручной запуск менеджером.

#### `grader_inputs`

- `grader_run_id UUID FK grader_runs`
- `participant_id UUID FK contest_participants`
- `submission_id UUID FK submissions`
- `judging_attempt_id UUID FK judging_attempts`
- `PRIMARY KEY(grader_run_id, judging_attempt_id)`.

Snapshot фиксирует неизменяемый набор данных, доступный конкретному Grader run. Он может включать посылки других участников, если это предусмотрено правами/настройками.

#### `grader_scores`

- `id UUID PK`
- `grader_run_id UUID FK grader_runs`
- `participant_id UUID FK contest_participants`
- `contest_problem_id UUID FK contest_problems`
- `score NUMERIC(20,8) NOT NULL`
- `metadata JSONB NULL`
- `created_at TIMESTAMPTZ NOT NULL`
- `UNIQUE(grader_run_id, participant_id, contest_problem_id)`.

Grader выдаёт итоговую оценку пользователя по задаче/contest problem. Он не изменяет базовые оценки Submission.

#### `score_publications`

- `id UUID PK`
- `contest_id UUID FK contests`
- `grader_run_id UUID FK grader_runs NULL`
- `source_type TEXT NOT NULL` — например `best_submission` или `grader`
- `status TEXT NOT NULL` — preview/ready/published/failed
- `created_at`, `published_at TIMESTAMPTZ NULL`.

#### `score_publication_values`

- `publication_id UUID FK score_publications`
- `participant_id UUID FK contest_participants`
- `contest_problem_id UUID FK contest_problems`
- `score NUMERIC(20,8) NOT NULL`
- `PRIMARY KEY(publication_id, participant_id, contest_problem_id)`.

Публикация score snapshot выполняется атомарно. `contests.current_score_publication_id` указывает на опубликованный набор. Участники не должны видеть смешанные результаты разных публикаций.

### 4.10. Audit / authorization outbox

Рекомендуются:

- `audit_logs` — кто, когда и что изменил; для менеджерских действий, публикации ревизий, перепроверок и Grader publish;
- `authorization_outbox` — синхронизация ролей/членств из PostgreSQL в OpenFGA;
- `system_settings` — конфигурация, которая должна быть изменяемой без пересборки приложения (секреты здесь не хранить).

---

## 5. OpenFGA authorization model

### 5.1. Объекты

- `user`
- `course`
- `contest`
- `problem`
- `contest_problem`
- `material`
- `submission`

### 5.2. Логические отношения

**Course:** owner, manager, teacher, student, viewer.  
**Contest:** manager, participant, viewer, course.  
**Problem:** owner, editor, viewer.  
**ContestProblem:** contest, problem.  
**Material:** contest.  
**Submission:** owner, contest.

Вычисляемые отношения:

- `can_view`;
- `can_edit`;
- `can_manage`;
- `can_submit`;
- `can_view_results`;
- `can_view_full_verdict`.

Точный DSL должен быть реализован и протестирован в OpenFGA. Не полагаться исключительно на frontend checks: каждый Backend endpoint повторно проверяет permission.

### 5.3. Contest-level permissions

- участник может видеть контест и его материалы, если имеет доступ;
- участник может отправлять решения только при открытой отправке и выполнении правил контеста;
- менеджер контеста всегда видит полный judging result и все логи, независимо от participant visibility settings;
- участник видит результаты только согласно `contest_problem.verdict_visibility`;
- права редактора Problem не должны автоматически давать доступ к приватным посылкам контеста, если это не предусмотрено отдельным отношением.

### 5.4. Visibility policy

Для каждой задачи в контесте настраивается видимость вердиктов/результатов. Предлагаемый набор уровней:

- `full` — полный результат;
- `verdict_only` — вердикт без подробных логов;
- `status_only` — только состояние проверки;
- `hidden` — результат скрыт.

Точный перечень полей, видимых участнику (score, test-by-test verdict, checker metadata, stdio, build log), должен быть зафиксирован в отдельном DTO. Backend формирует отфильтрованный ответ; frontend не должен самостоятельно скрывать секретные поля.

---

## 6. REST API Backend

API версионируется, например `/api/v1`. OpenAPI является контрактом Frontend ↔ Backend. Все endpoints проверяют session/principal, OpenFGA permissions и бизнес-ограничения.

### 6.1. Authentication

- `GET /auth/login`
- `GET /auth/callback`
- `POST /auth/logout`
- `GET /auth/session`
- `GET /api/v1/me`

### 6.2. Courses

- `GET /api/v1/courses`
- `POST /api/v1/courses`
- `GET /api/v1/courses/{courseId}`
- `PATCH /api/v1/courses/{courseId}`
- `GET /api/v1/courses/{courseId}/contests`
- `POST /api/v1/courses/{courseId}/contests`
- `DELETE /api/v1/courses/{courseId}/contests/{contestId}`
- endpoints для участников/ролей курса.

### 6.3. Contests

- `GET /api/v1/contests`
- `POST /api/v1/contests`
- `GET /api/v1/contests/{contestId}`
- `PATCH /api/v1/contests/{contestId}`
- `GET /api/v1/contests/{contestId}/problems`
- `POST /api/v1/contests/{contestId}/problems`
- `PATCH /api/v1/contests/{contestId}/problems/{contestProblemId}`
- `DELETE /api/v1/contests/{contestId}/problems/{contestProblemId}`
- `GET /api/v1/contests/{contestId}/participants`
- registration/join endpoints according to contest policy.

### 6.4. Problems and revisions

- `GET /api/v1/problems`
- `POST /api/v1/problems`
- `GET /api/v1/problems/{problemId}`
- `PATCH /api/v1/problems/{problemId}`
- `POST /api/v1/problems/import/polygon`
- `GET /api/v1/problems/{problemId}/revisions`
- `POST /api/v1/problems/{problemId}/revisions`
- `GET /api/v1/problems/{problemId}/revisions/{revisionId}`
- `POST /api/v1/problems/{problemId}/revisions/{revisionId}/validate`
- `POST /api/v1/problems/{problemId}/revisions/{revisionId}/publish`
- `POST /api/v1/problems/{problemId}/revisions/{revisionId}/bundle` (internal or signed download endpoint).

Bundle retrieval для Invoker должен быть service-authenticated и digest-verified. Публичный пользовательский endpoint не должен раскрывать внутреннюю структуру пакета.

### 6.5. Statements

- `GET /api/v1/problems/{problemId}/revisions/{revisionId}/statements`
- `PUT /api/v1/problems/{problemId}/revisions/{revisionId}/statements/{locale}`
- `POST /api/v1/problems/{problemId}/revisions/{revisionId}/statements/{locale}/render`
- `GET /api/v1/problems/{problemId}/revisions/{revisionId}/statements/{locale}/rendered`.

Создание/обновление statement выполняется в рамках draft revision. Опубликованную revision нельзя править на месте.

### 6.6. Theory materials

- `GET /api/v1/contests/{contestId}/materials`
- `POST /api/v1/contests/{contestId}/materials`
- `GET /api/v1/materials/{materialId}`
- `PATCH /api/v1/materials/{materialId}`
- `POST /api/v1/materials/{materialId}/revisions`
- `PUT /api/v1/contests/{contestId}/materials/{materialId}/revision`
- `POST /api/v1/materials/{materialId}/render`
- `GET /api/v1/materials/{materialId}/revisions/{revisionId}/rendered`.

### 6.7. Submissions

- `POST /api/v1/contests/{contestId}/problems/{contestProblemId}/submissions`
- `GET /api/v1/submissions/{submissionId}`
- `GET /api/v1/submissions/{submissionId}/attempts`
- `GET /api/v1/contests/{contestId}/problems/{contestProblemId}/submissions/me`
- `GET /api/v1/submissions/{submissionId}/artifacts/{artifactId}`
- `POST /api/v1/submissions/{submissionId}/rejudge` — manager-only, creates new attempt.

Upload may use multipart upload or presigned upload to S3. For large source archives prefer direct-to-object-storage upload with Backend-issued constrained upload token; finalization endpoint validates digest and size.

### 6.8. Scoreboard

- `GET /api/v1/contests/{contestId}/scoreboard`
- `GET /api/v1/contests/{contestId}/scoreboard/me`
- `GET /api/v1/contests/{contestId}/score-publications`

The response uses the currently published score snapshot. Manager view can include additional data and preview state.

### 6.9. Grader

- `POST /api/v1/contests/{contestId}/problems/{contestProblemId}/grader-runs` — manual trigger;
- `GET /api/v1/grader-runs/{graderRunId}`;
- `GET /api/v1/contests/{contestId}/grader-runs`;
- `POST /api/v1/grader-runs/{graderRunId}/publish` — publish a completed preview if policy allows;
- `POST /api/v1/grader-runs/{graderRunId}/cancel`.

Automatic trigger happens after a new Submission/base score if configured in the revision. Scheduled trigger is not supported.

### 6.10. Internal APIs

Internal routes must not be anonymously reachable through Nginx. Examples:

- Invoker event ingestion via gRPC;
- Invoker heartbeat/capacity registration;
- artifact finalize/metadata;
- Grader snapshot reads;
- Grader result submission;
- service health/metrics endpoints.

Service-to-service auth must be separate from end-user OIDC sessions.

---

## 7. gRPC / NATS integration with Invoker

### 7.1. NATS job delivery

Backend writes the judging task and an Outbox record in one DB transaction. Outbox Publisher sends a JetStream message when the queue policy allows assignment.

Job payload includes at least:

- `job_id`;
- `submission_id`;
- `judging_attempt_id`;
- `contest_id`;
- `contest_problem_id`;
- `problem_id`;
- `problem_revision_id`;
- `execution_format` and version;
- solution artifact reference + digest;
- task bundle reference + digest;
- resource policy;
- callback/correlation identifiers.

No large archive should be embedded directly in the NATS message.

### 7.2. gRPC events

Invoker reports:

- run started;
- build started/log chunks/finished;
- test started/finished;
- test verdict/score;
- subgroup score;
- problem base score;
- runner error;
- artifact references;
- run finished / timeout / cancelled.

Each event has `event_id`, `attempt_id`, `run_id`, monotonic sequence number, event timestamp and payload schema version. Backend deduplicates repeated event deliveries.

### 7.3. Cancellation

Backend records cancellation intent and sends control command through the agreed control channel. Invoker stops Lua Runner and all resources for that run, then reports terminal state. Backend must not treat a lost transport connection as proof that execution has terminated; leases/heartbeats and recovery are required.

### 7.4. Event/result ownership

Lua can set the base score for its Submission. Backend validates data shape, finite numeric values, allowed IDs/ranges, event ordering and attempt ownership. This validation is not algorithmic checker validation; correctness of task-specific scoring logic is the task author's responsibility.

Grader calculates a separate participant score from a fixed snapshot of Submission/JudgingAttempt data.

---

## 8. Grader architecture

### 8.1. Responsibility split

- Invoker / Standard Executor / Lua: execute a specific Submission and calculate its base score.
- Backend: store immutable Submission and JudgingAttempt facts.
- Grader: optionally derive a participant's final task score using multiple Submissions, potentially including other participants' Submissions.
- ScoreboardManager: serve the published result set.

### 8.2. Grader isolation

Grader scripts execute in a separate worker/runtime and do not connect directly to PostgreSQL. They call Backend internal APIs with a short-lived run-scoped capability token.

Grader input is an immutable snapshot that explicitly enumerates allowed submission/attempt IDs and relevant task/contest context. The grader cannot broaden its own access by guessing IDs.

Supported runtimes: Python or Lua (as agreed for grader scripts). The exact worker sandbox can reuse selected low-level Execution Engine capabilities but must be treated separately from Lua task runner execution.

### 8.3. Trigger modes

- automatic on a new Submission/base score when enabled;
- manual by manager;
- no scheduled mode.

### 8.4. Preview and publication

Preview writes a separate result set without changing currently published scores. Publish creates an immutable score publication and atomically switches the Contest's current publication pointer. During recomputation participants continue to see the previous published publication and may see a recalculation-in-progress indicator if contest policy requests it.

---

## 9. Frontend architecture

### 9.1. Technology and structure

- Vue + TypeScript + Vite.
- Vue Router recommended for page routing.
- Pinia recommended for shared UI/session state; server data should still be fetched through typed API clients and cached according to an explicit strategy.
- API types/client generated from OpenAPI where practical.
- Frontend does not own authorization decisions. It may hide unavailable actions for UX, but Backend must always enforce permissions.

Suggested structure:

```text
frontend/
├── src/
│   ├── app/
│   │   ├── router/
│   │   ├── providers/
│   │   └── layouts/
│   ├── api/
│   │   ├── generated/
│   │   └── clients/
│   ├── modules/
│   │   ├── auth/
│   │   ├── courses/
│   │   ├── contests/
│   │   ├── problems/
│   │   ├── statements/
│   │   ├── materials/
│   │   ├── submissions/
│   │   ├── judging/
│   │   ├── grader/
│   │   └── scoreboard/
│   ├── components/
│   ├── composables/
│   ├── styles/
│   └── main.ts
├── public/
└── vite.config.ts
```

### 9.2. Main navigation and pages

**Global:**

- login/session state;
- user profile;
- accessible courses;
- accessible contests;
- notifications/status messages.

**Course pages:**

- course list;
- course overview;
- ordered list of contests;
- course management and membership (according to permissions).

**Contest pages:**

- overview/rules;
- problems list;
- theoretical materials list and pages;
- scoreboard;
- participant's own submissions;
- contest manager settings.

**Problem pages:**

- statement with rendered HTML/math;
- examples and attachments;
- submission editor/upload;
- supported languages;
- submission history;
- judging progress and result.

**Problem authoring:**

- create Problem;
- import Polygon package;
- inspect import warnings/errors;
- edit draft revision;
- statement editor and preview;
- files/tests/languages/checker/runner configuration;
- validate/build bundle;
- publish revision;
- show warning if revision is used by contests;
- manager workflow to switch contest to new revision and optionally rejudge.

**Theory materials:**

- Markdown editor with preview and LaTeX math;
- LaTeX editor with preview;
- attach/detach material to Contest;
- order materials;
- revision history;
- attachment manager.

**Judging UI:**

- submission list;
- attempt history;
- current status and current test progress;
- verdict and score;
- build logs;
- test stdin/stdout/stderr (subject to visibility policy);
- artifacts download;
- manager-only rejudge and full diagnostic view.

**Grader UI:**

- grader enabled/configuration state;
- manual run;
- preview result;
- run progress and logs;
- publish preview;
- calculation history.

**Scoreboard:**

- participant rows (one user per row in current scope);
- problem columns;
- current published participant score;
- rank/order according to contest rules;
- optional detail drawer with score breakdown if visible.

### 9.3. Live status

For UI updates, use Server-Sent Events (SSE) or WebSocket via Backend. SSE is sufficient for one-way status updates if Frontend does not need a persistent bidirectional channel. Backend subscribes to internal events/state changes and emits only data visible to the current user.

Possible live events:

- submission queued;
- judging started;
- test N started;
- test N finished;
- build log available;
- judging completed;
- grader run started/completed;
- score publication changed.

Do not connect Frontend directly to NATS.

### 9.4. Rendering Statement and theory

Frontend receives sanitized HTML generated by Backend. It loads the pinned CSS and static assets for the selected KaTeX/MathJax renderer. It must not compile arbitrary raw LaTeX in a privileged client execution context.

Markdown source may contain mathematical formulas. Preview should use the same renderer version/configuration as final rendering to avoid differences.

### 9.5. Permission-aware UX

Frontend may request capability/permission DTOs from Backend to decide which controls to show. Examples:

- no edit control without `can_edit`;
- no submit button if user cannot submit or contest is closed;
- manager sees rejudge and full logs;
- participant sees only fields permitted by verdict visibility.

Backend response DTO must already be filtered. Hiding a field only in Vue is not a security control.

---

## 10. Key workflows

### 10.1. User login

1. Browser opens `/auth/login`.
2. Backend starts OIDC Authorization Code + PKCE flow.
3. Identity Provider authenticates user.
4. Backend validates callback/tokens.
5. IdentityManager resolves `(issuer, subject)` to local user.
6. Backend creates a server-side session and sets secure HttpOnly cookie.
7. Frontend loads `/api/v1/me`.

### 10.2. Create/publish Problem revision

1. Author creates Problem.
2. ProblemManager creates draft ProblemRevision.
3. Author imports Polygon package or edits scripted Lua package.
4. Files are stored as artifacts; source and manifest are validated.
5. Statement and assets are rendered/validated.
6. Problem bundle is built and digest calculated.
7. Revision becomes publishable/published.
8. If Problem is already used by ContestProblem records, managers are warned that a newer revision exists. No automatic revision switch.

### 10.3. Attach problem to Contest

1. Manager selects Problem.
2. Backend checks manager permission.
3. Manager selects a published ProblemRevision.
4. `contest_problems` stores the selected revision and per-contest settings.
5. Scoreboard and submission flows use that ContestProblem.

### 10.4. Attach theory material

1. Teacher/manager creates Material.
2. Selects Markdown or LaTeX.
3. Edits source and previews render.
4. Backend stores MaterialRevision and rendered artifact.
5. Manager attaches a specific revision to Contest and chooses position/visibility.
6. Frontend loads HTML and assets through permission-checked API.

### 10.5. Submit solution

1. Frontend requests upload authorization or uploads through Backend.
2. Backend checks OIDC session and OpenFGA permission.
3. Backend validates Contest state, deadline, language, file type/size, problem submission limit.
4. Source artifact is finalized and digest verified.
5. In one DB transaction Backend inserts Submission, initial JudgingAttempt, QueueEntry and Outbox event.
6. QueueManager waits for queue admission if required.
7. Outbox publisher publishes a runnable message to NATS.
8. Invoker receives job and fetches task/solution artifacts using service credentials.
9. Invoker sends progress/events over gRPC.
10. Backend stores immutable judging result and artifact refs.
11. Automatic Grader is triggered if enabled.
12. Scoreboard result is published according to the no-Grader or Grader path.

### 10.6. Manual rejudge

1. Manager chooses Submission(s).
2. Backend validates manager permission and selected target revision.
3. New JudgingAttempt is created; old attempt is untouched.
4. New attempt enters queue with reason and actor recorded.
5. New results are stored independently.
6. If Grader is enabled, a new Grader snapshot/run may be triggered.

### 10.7. Grader preview and publish

1. Manager triggers Grader or automatic trigger occurs after a new result.
2. Backend creates a stable snapshot enumerating eligible Submission/JudgingAttempt IDs.
3. Grader worker receives `grader_run_id` and run-scoped capability.
4. Grader requests data using Backend internal API; it has no DB credentials.
5. Grader returns participant scores.
6. Backend validates scope, IDs, finite values and permitted score ranges.
7. Preview stores a candidate result without updating `current_score_publication_id`.
8. Manager publishes preview or starts a publish-mode run according to policy.
9. Backend atomically changes the current publication pointer.

---

## 11. Queue and load management

Оценка исходной нагрузки: до 100 одновременно авторизованных пользователей; ожидаемый пик — до 2 посылок в секунду в начале/конце контеста. Это не означает, что все 100 пользователей одновременно отправляют решения.

Requirements:

- NATS JetStream;
- PostgreSQL Outbox;
- no priority queues;
- queue limits configured per Contest;
- submission limit per task/contest problem;
- Backend routes jobs to multiple Invokers;
- UI reflects waiting/queued/assigned/running/terminal states.

Open design points:

- `max_queued_submissions` per Contest or per Participant;
- whether currently running attempts count toward queue cap;
- exact unlock predicate for «пока у пользователя не протестируется хотя бы одна посылка»;
- maximum active jobs per Invoker;
- whether tests within one Standard attempt can run in parallel;
- retry policy after Invoker failure/lease expiry.

Do not silently choose one of these meanings in API or DB; keep configuration and logic explicit until resolved.

---

## 12. Security requirements

### 12.1. Browser / OIDC

- OIDC Authorization Code + PKCE;
- server-side token storage in BFF pattern;
- HttpOnly/Secure cookies;
- CSRF protection;
- strict redirect URI allowlist;
- issuer/audience/signature/expiry/nonce/state verification;
- no access tokens in localStorage;
- logout and server-side session revocation.

### 12.2. Authorization

- all resource endpoints check OpenFGA;
- backend additionally checks business state;
- do not trust user IDs/roles supplied by Frontend;
- internal service tokens differ from user sessions;
- Grader tokens restricted to one run snapshot;
- Invoker tokens restricted to judging event/artifact operations.

### 12.3. Uploads / files

- max upload size and file count;
- validate content and file names;
- protect against path traversal and symlink archive attacks in import/extraction;
- use content digest;
- S3 buckets private;
- presigned URLs short-lived and scoped;
- no public object listing;
- virus/malware scan may be added where appropriate.

### 12.4. Markdown / LaTeX

- sanitize rendered HTML;
- disallow script/event handler attributes;
- restrict external resource loads;
- use CSP in Nginx;
- LaTeX compiler runs with time/memory/file limits;
- only approved packages/macros;
- do not expose backend filesystem or shell through TeX commands.

### 12.5. Network

- PostgreSQL/NATS/OpenFGA/MinIO internal-only;
- no browser direct access to infrastructure;
- Invoker execution runtime follows separate sandbox specification;
- Grader runtime has no direct DB access;
- Nginx is only public listener.

---

## 13. Observability, logs and operations

Recommended:

- structured JSON logs;
- OpenTelemetry traces across HTTP → DB → outbox → NATS → Invoker events;
- Prometheus metrics;
- Grafana dashboards;
- readiness and liveness endpoints;
- healthchecks in compose;
- DB backup/restore procedure;
- MinIO backup/retention procedure;
- NATS JetStream persistence and monitoring;
- secret injection through files/environment manager, not committed compose values.

Key metrics:

- HTTP request rate/latency/error;
- OIDC callback failures;
- OpenFGA check latency/errors;
- queue length and oldest waiting age;
- submissions/sec;
- judging duration/verdict distribution;
- Invoker online count/capacity/heartbeat age;
- NATS pending/delayed/ack metrics;
- artifact upload/download errors;
- Grader run duration/failure count;
- score publication latency;
- DB connection pool and slow query metrics.

---

## 14. Suggested implementation milestones

1. Repository layout, compose, Nginx, Postgres migrations, health checks.
2. OIDC BFF, IdentityManager, session lifecycle.
3. OpenFGA integration, initial authorization model, memberships and authorization outbox.
4. CourseManager, ContestManager, individual participants.
5. ProblemManager, immutable revisions, ArtifactManager and bundle metadata.
6. StatementManager and DocumentRenderer.
7. MaterialManager: Markdown/LaTeX, revisions, contest attachments.
8. SubmissionManager and upload flow.
9. JudgingManager, queue model, Outbox → NATS, Invoker registration and gRPC events.
10. Judging result/history UI and live updates.
11. GraderManager, snapshot API, isolated Grader Worker and preview/publish.
12. ScoreboardManager and contest score publication.
13. Polygon import compatibility and integration tests.
14. Hardening, load tests, backup/restore and deployment documentation.

---

## 15. Decisions still requiring explicit specification

These points should remain visible in project planning; do not silently invent semantics.

1. OIDC provider(s), client registration, allowed issuers, account linking and session lifetime.
2. Whether multiple applications use one shared OpenFGA Store/model or separate Stores in one deployment.
3. Whether Course ↔ Contest remains many-to-many in final product (recommended structure).
4. Whether one Problem may be added more than once to the same Contest.
5. Polygon import channels: ZIP/package, Polygon API, or both; exact compatibility matrix.
6. Exact contest and problem submission limits, queue cap scope and release rule.
7. Invoker concurrency per instance and test-level parallelism.
8. Invoker lease duration, retry/recovery semantics and terminal behavior after node loss.
9. Exact participant-visible fields for each verdict visibility mode.
10. Grader execution scope: one ContestProblem or whole Contest; data visibility for other participants; score range and null/missing-score semantics.
11. Whether a Grader output always includes every participant or may omit participants (and what omission means).
12. Final renderer choice (KaTeX or MathJax), supported TeX subset and LaTeX compiler implementation.
13. Upload limits, artifact retention and S3 public/presigned/proxied delivery policy.
14. Exact Nginx/gRPC ingress: whether Invoker gRPC connects through Nginx or a separate private endpoint.
15. Whether Invoker and Grader Worker are deployed in the same compose project or as separate compose stacks on execution nodes.

