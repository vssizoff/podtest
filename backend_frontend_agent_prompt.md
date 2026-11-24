# Podtest: реализация Backend и Frontend тестирующей системы

Ты разрабатываешь Backend и Frontend тестирующей системы для соревнований и учебных занятий по программированию. Реализуй проект в ~/programming/podtest, соблюдая этот документ и архитектурную спецификацию `backend_frontend_architecture.md`.

Документ — техническое задание, а не предложение обсудить архитектуру. Если часть решений не определена, используй раздел «Открытые вопросы и правила работы с неопределённостью» в конце: не выдумывай скрытые продуктовые правила, делай безопасную минимальную реализацию и документируй предположение.

---

# 1. Краткий контекст системы

Система предназначена для задач программирования, соревнований и учебных курсов.

## 1.1. Форматы исполнения задач

Поддерживаются ровно два встроенных формата:

- `standard` — стандартные алгоритмические задачи, импортируемые из Codeforces Polygon;
- `scripted_lua` — пользовательский `runner.lua`, который определяет процесс тестирования через API Invoker.

HTTP-задачи, SQL-задачи, run-twice и иные специальные сценарии **не являются отдельными Backend execution formats**. Они реализуются на стороне Lua Runner готовыми библиотеками/шаблонами.

Все задачи имеют неизменяемые revisions. Контест закрепляет конкретную revision задачи. Изменение любого компонента задачи создаёт новую revision целиком.

## 1.2. Исполнение

Invoker-ов может быть несколько. Они забирают задания через NATS JetStream, а события и результаты отправляют Backend по gRPC. Invoker использует rootless Podman и общий Execution Engine для Polygon и Lua. Lua Runner выполняется в отдельном процессе, не имеет прямого доступа к Podman socket и управляет ресурсами через ограниченный RPC к Invoker.

Backend не выполняет код решений, `runner.lua`, checker или Grader внутри собственного процесса.

## 1.3. Оценивание

- Lua/Standard Executor выставляет базовую оценку конкретной посылки (`Submission base score`).
- Grader — отдельная система, которая по необходимости рассчитывает итоговую оценку пользователя на основании нескольких посылок этого или других участников.
- Grader не имеет прямого доступа к PostgreSQL; данные получает через ограниченный Backend API по snapshot и списку разрешённых ID.
- Grader запускается автоматически при новой посылке, если это настроено, или вручную менеджером. Запуск по расписанию не нужен.
- Preview не меняет опубликованные оценки. Публикация нового score snapshot выполняется атомарно.
- Повторная проверка Submission создаёт новый judging attempt. Старые attempts и их результаты не изменяются.

## 1.4. Контент и обучение

- Контесты объединяются в курсы.
- Контесты индивидуальные: один participant соответствует одному user. Командная модель не нужна.
- К контесту можно прикреплять задачи и теоретические материалы.
- Теория имеет два формата: Markdown и LaTeX. Markdown поддерживает LaTeX-формулы.
- Statement задачи — часть ProblemRevision. Изменение statement создаёт новую revision задачи.
- Теоретический материал имеет собственные revisions; его изменение не создаёт revision задачи.

## 1.5. Доступ

- Пользовательская аутентификация — OAuth2/OIDC.
- Авторизация объектов — OpenFGA в общей внутренней сети с системой и другими сервисами.
- PostgreSQL хранит доменные данные.
- Большие файлы/архивы/логи — S3-compatible storage (например, MinIO).

---

# 2. Технологический стек и общие ограничения

## Backend

- Go.
- HTTP router: `chi`.
- REST API с OpenAPI specification.
- PostgreSQL.
- NATS JetStream.
- gRPC + Protocol Buffers для взаимодействия с Invoker-ами.
- S3-compatible object storage.
- OpenFGA client.
- OIDC client / OAuth2 Authorization Code flow. (для разработки подними минимальный провайдер, в production - дам id и secret существующего)

## Frontend

- Vue 3 + TypeScript + Vite.
- Рекомендуется Vue Router.
- Рекомендуется Pinia для ограниченного shared UI/session state.
- Типизированный API client, по возможности генерируемый из OpenAPI.
- Не обращаться напрямую к PostgreSQL, NATS, OpenFGA, MinIO или Podman.

## Deployment

- Всё собирается и запускается через **Podman + `podman-compose`**.
- Nginx — единственная публичная точка входа.
- Nginx раздаёт Frontend static build и проксирует запросы к Backend.
- PostgreSQL, NATS, OpenFGA, MinIO и внутренние API не публикуются напрямую в интернет.
- Не требовать Docker daemon. Не использовать Docker Compose как обязательный runtime.

---

# 3. Обязательная работа перед реализацией

1. Изучи структуру репозитория, текущие исходники, конфигурации, миграции, compose-файлы и документацию.
2. Определи, какие части уже реализованы. Сохрани их, если они не противоречат этому ТЗ.
3. Составь короткий план по фактическому состоянию репозитория, затем приступай к реализации. Не ограничивайся только планом.
4. Не создавай параллельные дубли уже существующих пакетов/сервисов.
5. Согласуй названия, JSON conventions, error format, logging и config style с существующим кодом.
6. Не хардкодь credentials, tokens, passwords, issuer URLs или machine-specific paths.
7. Зафиксируй зависимости и версии в штатных файлах проекта.
8. Добавляй миграции, unit tests, integration tests и README для новых подсистем.
9. После каждого крупного этапа запускай форматтер, линтер/typecheck, unit tests и build.
10. В конце предоставь перечень реализованных функций, команды запуска/проверки и явно перечисли незакрытые пункты.

---

# 4. Архитектура Backend

## 4.1. Модульный монолит

Реализуй Backend как модульный монолит на Go. Manager-ы — отдельные внутренние packages/application services, а не отдельные микросервисы.

Один общий Backend binary может поддерживать режимы:

- `api` — HTTP API, OIDC/session endpoints и необходимый gRPC ingress;
- `worker` — Outbox publisher, NATS consumers, queue dispatch, внутренние фоновые задачи.

API и Worker собираются из одного Go module/image, но могут работать разными compose services. Database migrations выполняются отдельным one-shot service/command до readiness Backend.

### Обязательные менеджеры

#### AuthenticationManager

- OIDC Authorization Code flow;
- PKCE `S256`;
- state и nonce;
- callback и обработка ошибок;
- token exchange и валидация ID Token;
- session creation/logout/revocation;
- безопасная работа за Nginx reverse proxy;
- защита от open redirect и CSRF.

Предпочтительный браузерный режим — BFF: access/refresh tokens не сохраняются в localStorage. Frontend получает server-side session через `Secure`, `HttpOnly` cookie.

#### IdentityManager

- локальные users;
- mapping `issuer + subject` → `user_id`;
- отображаемое имя, email, avatar URL, status;
- обновление профиля по доверенным claims;
- не считать email идентификатором пользователя.

#### AuthorizationManager

- OpenFGA client;
- проверки permissions;
- batch checks;
- tuples/relationships write/delete;
- версионированная модель доступа;
- обработка ошибок OpenFGA;
- синхронизация изменений ролей через authorization outbox.

Каждый защищённый Backend endpoint проверяет права. Frontend checks — только UX, не security boundary.

#### CourseManager

- CRUD courses;
- владелец и менеджеры;
- список контестов курса;
- порядок контестов;
- архивирование;
- управление курсом и его участниками/ролями.

Связь Course ↔ Contest — many-to-many.

#### ContestManager

- CRUD contests;
- статус, начало/окончание;
- задачи контеста;
- настройки ContestProblem;
- закрепление ProblemRevision;
- участники;
- submission limits и queue settings;
- per-task verdict visibility;
- scoreboard settings;
- настройки доступа.

Контест индивидуальный. Создай `contest_participants(contest_id, user_id)` с уникальностью пары. Не создавай команды, team members или командные посылки.

#### ProblemManager

- CRUD Problem;
- Polygon import workflow;
- валидация импорта;
- immutable ProblemRevision;
- revision manifest;
- исходный import artifact;
- generated execution bundle;
- digest и revision integrity;
- получение точной revision bundle для Invoker;
- предупреждения об опубликованной новой revision, если старые revisions используются в контестах.

Execution formats строго `standard` и `scripted_lua`. Не добавляй HTTP/SQL/run-twice как отдельные форматы.

Для Polygon import не утверждай поддержку каждой функции Polygon без проверки. Добавь явную compatibility layer, warnings/errors и документацию о поддерживаемых возможностях. ZIP/package и Polygon API оставь конфигурируемыми/отдельными importer adapters, если в репозитории или доступной спецификации нет принятого решения о единственном способе.

#### StatementManager

- statement variants/locales;
- source text/resources;
- редактор и preview endpoints;
- рендеринг в HTML;
- assets references;
- statement принадлежит ProblemRevision;
- изменение statement в published revision запрещено: создаётся новая ProblemRevision.

#### MaterialManager

- теория, прикрепляемая к Contest;
- форматы `markdown` и `latex`;
- Markdown с LaTeX formulas;
- отдельные MaterialRevision;
- attachments/images;
- порядок и visibility в Contest;
- rendered HTML/cache metadata;
- изменение Material не создаёт ProblemRevision.

#### DocumentRenderer

- backend-side Markdown rendering;
- математические формулы;
- самостоятельный LaTeX rendering;
- sanitizer;
- безопасные asset references;
- renderer version/digest cache;
- ограниченное время/память/доступные пакеты для TeX compilation;
- запрет произвольного shell execution и небезопасных TeX commands.

Выбери/зафиксируй одну основную реализацию KaTeX или MathJax для вывода, после чего отдавай Frontend HTML и стандартные CSS/JS assets выбранной библиотеки. Не заставляй браузер компилировать исходный TeX в привилегированном контексте.

#### LanguageManager

- builtin Polygon languages;
- user-defined languages;
- templates;
- custom Containerfile/Dockerfile metadata;
- build/run commands;
- связка языковой конфигурации с ProblemRevision.

Автор может использовать стандартные языки Polygon и добавлять собственные языки. Build commands и Containerfile — недоверенная конфигурация, исполняемая только через Invoker sandbox, не на Backend.

#### SubmissionManager

- source upload;
- digest/size verification;
- validation file/language/problem/contest;
- submission limits;
- создание Submission;
- вызов JudgingManager/создание первичного JudgingAttempt;
- история посылок;
- выдача приватных source/result artifacts после authorization.

Submission — неизменяемая запись. Источник хранить в object storage.

#### JudgingManager

- judging attempt lifecycle;
- queue request;
- assignment/cancellation;
- ручная rejudge;
- target ProblemRevision;
- попытка повторной проверки — новый attempt;
- старые attempts неизменяемы;
- хранение статуса/verdict/base score;
- запуск Grader, если настроен automatic-on-submission.

#### QueueManager

- queue_entries в PostgreSQL;
- Transactional Outbox;
- публикация runnable jobs в NATS JetStream;
- no priorities;
- per-contest queue controls;
- ожидание допуска к очереди, если лимит достигнут;
- assignment и повторная обработка согласно lease/recovery policy.

Уже заданные правила: max queued submissions на уровне Contest, если лимит превышен, новая отправка ожидает до тестирования хотя бы одной посылки пользователя; max submissions per task; приоритетов нет. Точные счётчики/события для этих правил не выдумывать: вынести в конфигурацию и TODO с ясными именами, если решение ещё не принято.

#### InvokerManager

- реестр экземпляров Invoker;
- heartbeat;
- online/offline;
- capabilities/protocol versions;
- capacity/load;
- assignment;
- lease/recovery;
- сопоставление judging attempt ↔ Invoker.

В системе может быть несколько Invoker на разных хостах. Все поддерживают `standard` и `scripted_lua`. Архитектура допускает несколько одновременных прогонов.

#### JudgingEventManager

- gRPC event intake;
- event idempotency;
- sequence validation;
- correlation по `run_id`, `submission_id`, `judging_attempt_id`;
- запись status/test verdict/subgroup score/problem base score;
- artifact references;
- RunnerError/RunTimeout/RunFinished;
- запрет изменения terminal attempt запоздавшим событием.

События должны иметь уникальный `event_id`, `attempt_id`, `run_id`, sequence number и schema version.

#### GraderManager

- grader configuration в task revision;
- trigger automatic on submission и manual by manager;
- scheduled mode не реализовывать;
- snapshot набора Submission/JudgingAttempt IDs;
- Grader Python или Lua runtime;
- Grader не имеет прямого доступа к БД;
- ограниченный Backend API и run-scoped capability token;
- Grader может читать данные нескольких посылок этого или других участников, только если они входят в разрешённый snapshot;
- preview и publish modes;
- неизменяемая история grader runs;
- валидация score payload;
- атомарная публикация нового score snapshot.

Логика оценивания:

- Lua/Standard Executor выставляет базовый score отдельной Submission;
- если Grader отключён, базовый score агрегируется по стандартной политике, исходная рекомендация — максимум среди посылок пользователя по задаче;
- если Grader включён, он возвращает итоговую оценку пользователя по ContestProblem на основе snapshot множества посылок;
- Grader не изменяет judging attempt/base score;
- preview не меняет опубликованную таблицу;
- publish атомарно переключает current score publication.

#### ScoreboardManager

- participant/problem score matrix;
- базовая политика без Grader;
- published Grader output при включённом Grader;
- rank/ordering;
- contest scoreboard visibility;
- публичный и manager views;
- не пересчитывает judging results.

#### ArtifactManager

- private S3 storage;
- upload/download metadata;
- content digest/size/media type;
- revision bundles;
- solution sources;
- build/test/runner/checker/grader logs;
- stdin/stdout/stderr;
- material/statement attachments.

Большие файлы не хранить в PostgreSQL и не передавать целиком NATS/gRPC. Использовать upload flow/presigned URLs и ссылки на artifacts.

#### AuditManager

Записывать важные административные действия: изменение ролей, публикация revisions, смена revision у ContestProblem, rejudge, запуск/публикация Grader, изменение contest settings.

---

# 5. Модель данных и PostgreSQL migrations

Создай миграции, foreign keys, indexes, unique constraints и CHECK constraints. UUID для сущностей, `TIMESTAMPTZ` для дат. Для score использовать `NUMERIC`, не binary float в PostgreSQL. Не делать все поля одной JSONB колонкой.

Обязательные таблицы и связи:

## 5.1. Identity

- `users(id, display_name, email, avatar_url, status, created_at, updated_at)`;
- `user_identities(id, user_id, issuer, subject, created_at)`, `UNIQUE(issuer, subject)`;
- `auth_sessions(id, session_token_hash, user_id, created_at, expires_at, last_seen_at, revoked_at)` — при BFF sessions.

## 5.2. Courses / Contests

- `courses`;
- `contests`;
- `course_contests(course_id, contest_id, position, added_at)` — many-to-many;
- `contest_settings`;
- `contest_participants(id, contest_id, user_id, status, registered_at)`, unique `(contest_id,user_id)`;
- domain membership tables для course/contest roles, если PostgreSQL выбран источником canonical relationship facts.

Никаких team tables: контесты индивидуальные.

## 5.3. Problems

- `problems`;
- `problem_revisions` с `revision_number`, `execution_format`, format version, runner API version, manifest JSONB, source/bundle artifact IDs, digest, status;
- `problem_statements` по locale, привязанные к revision;
- `contest_problems` с закреплённым `problem_revision_id`, order, max score, submission limit, verdict visibility;
- `languages`;
- `problem_revision_languages` с build/run config и custom Containerfile artifact.

Revision immutable после publication.

## 5.4. Materials

- `theory_materials`;
- `material_revisions` с `format IN ('markdown','latex')`, source, digest, rendered artifact, renderer version/status;
- `contest_materials` с `contest_id`, `material_revision_id`, `position`, visibility.

## 5.5. Submission / Judging

- `submissions` с `contest_problem_id`, `participant_id`, `submitted_by`, `problem_revision_id`, language, source artifact, digest, timestamp;
- `judging_attempts` с `submission_id`, attempt number, target problem revision, status, verdict, base score, trigger actor/reason, retry link, invoker and timestamps;
- `test_results` с test ID, verdict, score, `checker_metadata`;
- `subgroup_results`;
- `queue_entries`;
- `invokers`;
- `judging_events`;
- `outbox_events`;
- `inbox_events`/deduplication records.

## 5.6. Artifacts

- `artifacts` (storage key, digest, size, media type/type, created_at);
- `attempt_artifacts` (attempt, artifact, kind, optional test_id).

## 5.7. Grader / publication

- `grader_runs` с contest/problem scope, trigger type (`automatic_submission`, `manual`), mode (`preview`, `publish`), status, snapshot ID/digest;
- `grader_inputs` с `grader_run_id`, participant, submission, judging attempt;
- `grader_scores` с participant, contest_problem, score, metadata;
- `score_publications`;
- `score_publication_values`;
- `contests.current_score_publication_id` (добавить FK подходящей миграцией).

Score publication — immutable snapshot. Публикация переключается одной транзакцией.

## 5.8. Дополнительные таблицы

- `authorization_outbox`;
- `audit_logs`;
- системные настройки без секретов.

### DB constraints/index guidance

Добавь индексы минимум для:

- уникального issuer+subject;
- contest/user participant;
- course/contest mappings и positions;
- problem/revision number;
- contest problem position;
- submissions по participant + contest_problem + submitted_at;
- attempts по submission + attempt_number/status/created_at;
- queue entries по status + created_at, contest и participant;
- events по attempt + sequence и unique event id;
- artifacts по storage key/digest;
- grader runs по contest/status/created_at;
- score publications по contest/status;
- published current score pointer.

Проверяй, что FK/index strategy не создаёт ненужных каскадных удалений истории. Не удалять Submission/JudgingAttempt/публикации каскадно при удалении пользователя или контеста; использовать archive/soft delete или explicit retention policy.

---

# 6. Authorization: OpenFGA

Используй OpenFGA в существующей общей внутренней сети. Не разворачивай второй OpenFGA без необходимости (разверни в development среде).

Подготовь версионированный model file и тесты отношений. Логические типы:

- `user`;
- `course`;
- `contest`;
- `problem`;
- `contest_problem`;
- `material`;
- `submission`.

Логические роли/отношения:

- Course: owner, manager, teacher, student, viewer;
- Contest: manager, participant, viewer, course;
- Problem: owner, editor, viewer;
- ContestProblem: contest, problem;
- Material: contest;
- Submission: owner, contest.

Вычисляемые permissions:

- `can_view`, `can_edit`, `can_manage`, `can_submit`, `can_view_results`, `can_view_full_verdict`.

Реализуй единый AuthorizationManager, не вызывай OpenFGA client хаотично из HTTP handlers.

Если PostgreSQL хранит source of truth membership/roles, синхронизируй tuples через authorization outbox. Напиши retry механизм. Ошибка OpenFGA при защищённом действии не должна приводить к allow.

Manager всегда имеет полный доступ к verdict/logs для управляемого Contest. Участнику показывай поля только согласно per-task verdict visibility policy.

---

# 7. REST/OpenAPI requirements

Все публичные HTTP endpoints находятся под `/api/v1`, OIDC auth endpoints могут быть под `/auth`. Используй единый error format, pagination, validation и request IDs.

Минимальные группы endpoints:

## Auth

- `GET /auth/login`;
- `GET /auth/callback`;
- `POST /auth/logout`;
- `GET /auth/session`;
- `GET /api/v1/me`.

## Courses

- list/create/get/update/archive;
- list/add/remove/reorder contests;
- management of course roles/members.

## Contests

- list/create/get/update/archive;
- settings;
- problems list/add/update/remove/revision switch;
- participants list/register/join (according to configured access policy);
- materials list/attach/detach/reorder;
- scoreboard and manager views.

## Problems

- create/get/update;
- Polygon import;
- list revisions;
- create draft revision;
- validate and publish revision;
- retrieve statement and rendered statement;
- manage files/languages/manifest;
- produce protected execution bundle reference for Invoker.

## Materials

- CRUD materials;
- create revision;
- update source;
- render preview;
- attach fixed revision to Contest;
- reorder and visibility.

## Submissions

- create/upload submission;
- list current user's submissions;
- get submission;
- list attempt history;
- get allowed result/log/artifact;
- manager rejudge endpoint.

## Grader

- automatic trigger from judging completion if configured;
- manual create grader run;
- get run/status/logs;
- preview;
- publish preview;
- cancel run where safe.

## Scoreboard

- public/current published scoreboard;
- own row;
- manager full scoreboard and calculation state.

Internal APIs must be isolated/authenticated:

- Invoker event/heartbeat API via gRPC;
- Grader snapshot reads;
- Grader result submission;
- artifact finalize/upload metadata.

Generate and maintain OpenAPI. Do not hand-maintain divergent API DTOs in Backend and Frontend.

---

# 8. NATS, Outbox and Invoker integration

## 8.1. Submission transaction

Creating a runnable submission must be transactional:

1. validate user/contest/problem/language/limits;
2. ensure upload artifact exists and digest is verified;
3. insert Submission;
4. insert initial JudgingAttempt;
5. insert QueueEntry;
6. insert Outbox event;
7. commit.

Outbox publisher delivers eligible jobs to NATS JetStream. Consumer handling must be idempotent.

## 8.2. Job payload

Include:

- job/run id;
- submission id;
- judging attempt id;
- contest/contest problem/problem/revision IDs;
- execution format and versions;
- source artifact ref/digest;
- task bundle ref/digest;
- resource policy;
- event correlation identifiers.

Do not embed large archives in messages.

## 8.3. Invoker events

At minimum support:

- RunStarted;
- BuildStarted;
- BuildLogChunk / BuildFinished;
- TestStarted / TestFinished;
- test verdict/score;
- SubgroupScore;
- ProblemScore (base score for this submission);
- ArtifactCreated;
- RunnerError;
- RunTimeout;
- RunCancelled;
- RunFinished.

Every event has event id, attempt/run IDs, sequence number, schema version and timestamp. Deduplicate and reject late events after terminal attempt unless explicit protocol recovery permits them.

Task bundle/solution source are fetched from object storage using scoped service authorization and verified by digest.

## 8.4. Reliability

Do not assume a lost TCP/gRPC connection means the Invoker process stopped. Use heartbeat/lease, explicit cancellation and recovery state. Do not allow two active Invokers to own the same attempt without a fencing/lease generation. Define retry semantics in code and tests; use attempt/run generation to reject stale event streams.

---

# 9. Grader integration

Implement Grader as an independently executed worker/runtime, not as a Backend package executing user script in-process.

- Grader runtime: Python or Lua.
- No PostgreSQL connection/credentials in Grader container.
- Backend creates immutable grader snapshot.
- Grader receives `grader_run_id`, snapshot digest, permitted participant/submission/attempt IDs, and short-lived capability token.
- Grader fetches data using scoped Backend internal API.
- Grader can analyze multiple submissions from the same or different participants if they are inside the snapshot and allowed by configuration.
- Grader returns scores for participants and relevant ContestProblems plus optional metadata/log artifact.
- Backend validates IDs, scope, finite score values and score ranges.
- Preview stores separate candidate results.
- Publish atomically creates new immutable publication and changes Contest pointer.
- Never mutate Submission base scores or old JudgingAttempts.
- Trigger modes: automatic on new submission/base result and manual by manager. No scheduler.

Default no-Grader score aggregation should be highest base score among the user's submissions for that task, unless a product configuration explicitly changes the policy. Keep this policy in one Scoreboard/scoring module, not scattered SQL in handlers.

---

# 10. Frontend requirements

## 10.1. Global app

Implement:

- Vue + TypeScript + Vite;
- route-based layout;
- authenticated user/session state;
- API client with typed DTOs and unified error handling;
- loading/error/empty states;
- responsive UI;
- permission-aware navigation and actions;
- no direct database/queue/OpenFGA/storage connections.
- Use my ui components (like in ~/programming/inf54/frontend/src/components/termui)

Use same-origin `/api` and `/auth` through Nginx. Session cookie is automatically sent; use CSRF token/header for unsafe requests as Backend requires.

## 10.2. Pages

### Auth

- login entry;
- session expiry/unauthorized state;
- logout;
- profile.

### Courses

- course catalog available to user;
- course details;
- contests in course, ordered;
- manager controls for course information and membership/roles.

### Contests

- contest overview/status/rules;
- problems list;
- theory materials;
- scoreboard;
- participant area;
- contest settings for managers;
- participants management.

### Problem viewing

- statement rendered from Backend HTML;
- math formulas;
- images/attachments;
- sample input/output;
- supported languages;
- submit solution form;
- submission history;
- current judging status and verdict.

### Problem authoring

- create Problem;
- Polygon import/upload;
- import diagnostics and compatibility warnings;
- revision list;
- draft editing;
- statement editor/preview;
- test and resource file management;
- language/build configuration;
- `standard` vs `scripted_lua` selection;
- runner.lua and task file management for scripted format;
- validation/publish;
- warning when an updated Problem is attached to existing Contests;
- manager action to switch selected ContestProblem revision and optionally trigger rejudge.

### Theory materials

- create Material;
- Markdown/LaTeX format selection;
- editor and preview;
- inline/display math preview;
- attachments;
- revision history;
- attach revision to one or more Contests;
- reorder and visibility controls.

### Submissions/judging

- upload/source submit;
- list own submissions;
- status progression;
- attempt history;
- build logs;
- tests progress (`testing test N`);
- verdict/score display;
- permitted stdio/log artifacts;
- manager-only full diagnostics and rejudge.

### Grader

- indicate if Grader enabled;
- manual run action for managers;
- preview candidate score table;
- run status/errors/logs;
- publish action;
- history of grader runs and score publications.

### Scoreboard

- individual participants only (one user per participant);
- one row per user;
- one column per ContestProblem;
- published participant score;
- rank/order;
- drill-down only for permitted details.

## 10.3. Live updates

Use SSE or WebSocket from Backend. Prefer SSE for one-way judging and Grader status events unless a genuine bidirectional interaction is required.

Possible events:

- submission accepted/queued;
- judging started;
- test started/finished;
- build log/artifact available;
- attempt completed/failed/cancelled;
- Grader started/completed;
- score publication changed.

Backend must filter events by resource visibility and user permissions. Frontend never subscribes directly to NATS.

## 10.4. Statement/material rendering

Backend returns sanitized rendered HTML and renderer asset/version metadata. Frontend loads pinned KaTeX/MathJax CSS/resources. Markdown preview must use same renderer pipeline/configuration as final output. Do not use `v-html` on unsanitized arbitrary source.

## 10.5. Verdict visibility

Backend returns participant-safe DTOs based on per-task visibility setting. Frontend may hide/show UI controls based on permission, but must not receive secret logs/metadata if user is not allowed to view them. Contest managers always receive complete result and diagnostics.

---

# 11. Nginx and Podman Compose

## 11.1. Nginx

Nginx must:

- serve Frontend build at `/`;
- SPA fallback to `/index.html` for client-side routes;
- reverse proxy `/api/` and `/auth/` to `backend-api`;
- proxy SSE without buffering (if SSE selected), set appropriate read timeout;
- optionally proxy Invoker gRPC with `grpc_pass` on a dedicated internal/secured route/listener;
- set request size/timeouts for source and task uploads;
- set secure headers/CSP;
- pass original scheme/host/remote address correctly;
- not expose `/internal/*`, metrics, debug, pprof or admin ports publicly.

Nginx image should be built through a multistage build:

1. Node image installs lockfile dependencies and runs Vite production build;
2. Nginx runtime image copies `dist/` and configuration;
3. no Node runtime is required in production image.

## 11.2. Compose services

At minimum define:

- `nginx`;
- `backend-api`;
- `backend-worker`;
- `postgres`;
- `nats` with JetStream persistence;
- `minio` or selected S3-compatible backend;
- `openfga`;
- `db-migrate` one-shot;
- OpenFGA model migration/loader one-shot;
- `grader-worker` (or separate profile if not implemented in first slice).

Invoker deployment can be in the main compose for development or separate compose stack on execution nodes. If containerized, mount only configured rootless Podman socket and workspace directories; verify UID/GID, socket access and user namespace mapping.

Compose requirements:

- all app images built from repository Dockerfiles;
- named persistent volumes for Postgres, NATS, MinIO and required state;
- internal networks for data services;
- healthchecks and `depends_on` readiness strategy where supported;
- `.env.example` with no real secrets;
- production secrets injected securely;
- resource requests/limits where supported;
- log rotation;
- migration command documented;
- no mandatory Docker daemon.

Document these operations:

```bash
podman-compose build
podman-compose up -d
podman-compose ps
podman-compose logs -f backend-api
podman-compose down
```

`down` must not delete persistent volumes by default. Destructive volume removal requires an explicit separate command and warning.

---

# 12. Security requirements

- OIDC Authorization Code + PKCE;
- secure BFF session, HttpOnly/Secure cookie;
- CSRF for state-changing requests;
- strict CORS (prefer same origin through Nginx);
- validate all request data on Backend;
- OpenFGA checks on every protected resource action;
- no trust in client-supplied role/user_id;
- private object buckets and short-lived scoped artifact access;
- archive path traversal/symlink protection for Polygon and solution archives;
- sanitize Markdown/LaTeX rendered HTML;
- CSP and safe external resource policy;
- rate limits for auth/upload/submission endpoints;
- service-to-service auth separate from user session;
- run-scoped Grader capability token;
- Invoker and Grader never get unrestricted DB credentials;
- never expose NATS/Postgres/OpenFGA/MinIO/Podman socket to browser.

---

# 13. Tests and acceptance criteria

## 13.1. Backend tests

- OIDC callback validates issuer/audience/nonce/state and rejects invalid tokens;
- local user mapping uses `(issuer, subject)`;
- unauthorized user cannot access another user's private submission;
- OpenFGA error fails closed;
- course can contain multiple contests and contest can be reused by multiple courses;
- participant uniqueness `(contest_id,user_id)` is enforced;
- no team functionality is introduced;
- ProblemRevision cannot be mutated after publication;
- ContestProblem pins an exact revision;
- changing Problem revision does not silently update ContestProblem;
- Statement change creates a new ProblemRevision;
- Theory Material revision is independent from ProblemRevision;
- Markdown with LaTeX formulas renders;
- standalone LaTeX renders in backend;
- unsafe HTML/TeX constructs are rejected/sanitized;
- submission creation checks contest status, problem availability, language and limits;
- Submission + JudgingAttempt + QueueEntry + Outbox are transactionally created;
- Outbox publisher retry does not create duplicate judging jobs;
- duplicate Invoker event is idempotent;
- stale event cannot overwrite terminal attempt;
- rejudge creates a new attempt and preserves old results;
- Grader snapshot is immutable;
- Grader cannot fetch IDs outside snapshot;
- Grader has no DB connection;
- preview leaves current publication unchanged;
- publish switches score publication atomically;
- default no-Grader aggregation selects best base score;
- manager sees full verdict; participant receives filtered DTO.

## 13.2. Frontend tests

- login/logout/session state;
- route guards and error states;
- course/contest pages;
- task statement rendering and formulas;
- Markdown/LaTeX material editor preview;
- Polygon import feedback;
- revision editor and publication workflow;
- submission upload and history;
- live judging status updates;
- verdict visibility on participant and manager views;
- Grader preview/publish workflow;
- individual scoreboard (no team rows).

## 13.3. Deployment acceptance

From a clean checkout with documented env file:

1. `podman-compose build` builds all configured images;
2. `podman-compose up -d` starts services;
3. Nginx serves Vue static app;
4. `/api/v1/health/ready` (or agreed endpoint) is reachable through Nginx;
5. OIDC config is loaded from environment/secrets;
6. migrations run successfully;
7. PostgreSQL/NATS/OpenFGA/MinIO are not publicly exposed;
8. persistent data survives container recreation;
9. logs are accessible via compose;
10. documented shutdown does not delete data volumes.

---

# 14. Open questions and rules for unresolved details

The following are not fully specified. Do not invent a product rule and silently hardcode it.

1. OIDC provider URL/client registration and account linking policy.
2. Whether the shared OpenFGA deployment uses one common Store or a dedicated Store for this application.
3. Exact queue cap scope (per Contest or per user), whether running jobs count, and precise unlock condition.
4. Exact max submissions semantics (per user/problem/contest, including rejected/cancelled submissions).
5. Invoker max concurrency, test-level parallelism and lease/retry timeouts.
6. Polygon import transports and exact supported Polygon feature matrix.
7. Full participant-visible verdict field matrix.
8. Whether one Problem may appear multiple times in one Contest.
9. Whether Grader operates per ContestProblem or whole Contest and exact other-participant data visibility.
10. Whether missing Grader output means zero, unchanged, omitted or failure. Do not infer this.
11. Score bounds/precision and treatment of partial test/subgroup reports.
12. Final choice of Markdown/LaTeX renderer and supported TeX package/macro subset.
13. Upload/file/stdio retention limits.
14. Whether Invoker gRPC passes through Nginx or uses a dedicated private endpoint.
15. Final production placement of Invokers and Grader Workers (same host vs separate execution nodes).

For an implementation slice that touches an unresolved item, define an interface/configuration point and document the assumption. Ask only if an irreversible schema/product decision is unavoidable. Prefer forward-compatible schema and explicit validation over guessed behavior.

---

# 15. Expected deliverables

1. Go Backend source with modular managers and API/worker entry points.
2. Vue + TypeScript + Vite Frontend.
3. OpenAPI specification and typed Frontend API client.
4. PostgreSQL migrations and development seed data (no production secrets).
5. OpenFGA model files and authorization tests.
6. NATS JetStream configuration and Outbox/Inbox implementation.
7. gRPC Protocol Buffers for Invoker integration (or documented compatible stub until Invoker contract is merged).
8. Grader internal API and isolated worker integration/stub.
9. Artifact storage abstraction and MinIO-compatible implementation.
10. Nginx config and multistage frontend/nginx build.
11. `compose.yaml`, development overrides/profiles, `.env.example`.
12. Unit and integration tests.
13. README with `podman-compose` build/up/down, migrations, OIDC setup, OpenFGA model loading, MinIO initialization and troubleshooting.
14. A concise implementation report listing completed items, test/build results and remaining open questions.

**Definition of done:** Backend and Frontend build; deployment starts using Podman Compose; Nginx serves the SPA and proxies Backend; OIDC session flow works with configured provider; authorization is enforced through OpenFGA; courses, individual contests, tasks/revisions, statements, materials, submissions, judging integration, Grader preview/publish and scoreboard have a coherent end-to-end API/UI path; automated tests cover critical invariants above.
