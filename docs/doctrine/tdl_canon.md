# TDL Canon — Decision Process and Anti-Patterns

Source-of-truth references at the end. This is the thought process
LeadKart-Go MUST follow. Drift = finding. Arch tests in
`internal/architecture/tdl_strict_arch_test.go` enforce the
mechanical subset of these rules; the rest is review-time judgment.

---

## Preamble — TDL is opinionated; "Clean Architecture" alone is not enough

TDL's stack (DDD Lite + Hexagonal + CQRS + EDA) overlaps with generic Clean Architecture but takes **specific positions** that diverge from typical implementations:

- They **reject mocks** in favour of fakes that pass the same contract test suite as the real adapter.
- They **reject DI containers** in favour of manual constructor wiring (Mat Ryer 2024 canon).
- They **reject "service layers"** above command handlers — the handler IS the orchestrator and IS the use-case contract.
- They **reject generic setters/getters** — aggregates expose only behaviour methods (`ScheduleTraining()`, not `SetState()`).
- They **reject distributed transactions** unconditionally — wrong boundaries are an architectural problem, not a 2PC problem.
- They **reject schema-first / CRUD-first** design — model the behaviour, then choose the table.

Below, every decision domain is captured as `Position / Why / Anti-pattern / How to enforce`.

---

## 1. Layering — what may import what

### Position
Four layers per bounded context, with strict inward-only dependency:

```
internal/<module>/
├── domain/        # pure business logic — entities, VOs, repo interfaces
├── app/           # use cases — command + query handlers
│   ├── command/
│   └── query/
├── ports/         # inbound — HTTP server, gRPC server, event subscribers
└── adapters/      # outbound — pg repos, message publishers, gRPC clients
```

Interfaces live with their **consumer**, not their producer. Domain repository interfaces live in `domain/<aggregate>/repository.go`. Handler-local outbound dependencies (e.g. `UserService`, `TrainerService` in Wild Workouts `command/services.go`) live next to the handler that needs them.

### Why
TDL: *"outer layers (implementation details) can refer to inner layers (abstractions), but not the other way around"* ([introducing-clean-architecture](https://threedots.tech/post/introducing-clean-architecture/)).

The rule prevents the bug class where a refactor of the SQL adapter (changing a column name, swapping pgx for sqlx) forces a recompile of the domain — which is supposed to be persistence-ignorant. Consumer-side interfaces ("accept interfaces, return structs" — Cheney) keep the producer free to evolve.

### Anti-pattern
- `domain/` importing `pgx`, `pgtype`, sqlc-generated row types — couples the model to the driver.
- `app/` importing `adapters/db` (sqlc rows) or concrete repo structs — forces the use case to depend on a specific persistence shape.
- Interfaces defined in `ports/` or `adapters/` ("producer-side") — forces consumers to import the producer.
- A single shared `interfaces/` package — kills the consumer-side discipline and produces god-package coupling.

### How to enforce
- Walk every `.go` file under `app/`; fail on any import of `pgx`, `pgxpool`, `pgtype`, `adapters/db`, or the parent `adapters/` package. (LeadKart already has this — `TestArch_AppDoesNotImportForbidden` per ADR 0047.)
- Walk every `.go` file under `domain/`; fail on any non-stdlib + non-domain import except a closed allowlist (uuid, time, money lib).
- Walk every interface declaration; fail if it lives in `adapters/` or `ports/`.

---

## 2. Aggregate factories + mutators

### Position
Aggregates have **private fields**, exposed only through **behaviour methods**. Two construction paths:
- `NewX(...) (*X, error)` — for fresh aggregates; validates all invariants.
- `UnmarshalXFromDB(...) *X` (or `NewXFromDB`) — for rehydration; skips business validation because DB invariants already hold.

State transitions are methods with business names:
```go
func (h *Hour) ScheduleTraining() error {
    if !h.IsAvailable() {
        return ErrHourNotAvailable
    }
    h.availability = TrainingScheduled
    return nil
}
```

### Why
TDL: *"The only public API of the object should be methods describing behaviors"* ([ddd-lite-in-go-introduction](https://threedots.tech/post/ddd-lite-in-go-introduction/)).

Validation in constructors guarantees the "always keep a valid state in memory" rule. Once an `*Hour` exists, it is — by construction — internally consistent. Setters break this: `h.SetState("scheduled")` lets any caller put the aggregate in any state regardless of business rules.

The separate rehydration ctor exists because the DB is the source of truth for already-persisted state; re-running ctor validation on rehydration would reject valid historical records when business rules evolve.

### Anti-pattern
- `SetX()` / `GetX()` pairs on aggregates — leaks state instead of exposing behaviour.
- Public fields on aggregates — same problem, even worse.
- A single ctor for both creation and rehydration — validation either rejects legitimate historical state, or is removed entirely (defeating the point).
- A `MustNewX` in a request path — `panic` in domain construction must be init/test-only.

### How to enforce
- For every type implementing `domain.AggregateRoot` (or matching `internal/<module>/domain/<name>/<name>.go`), assert all struct fields are unexported.
- Forbid exported methods on aggregates whose name starts with `Set` (with an allowlist for legitimate domain verbs that happen to begin with "Set").
- Require each aggregate package to expose **both** `NewX(...) (*X, error)` and a rehydration ctor (naming: `UnmarshalXFromDB` or `NewXFromDB`).
- Forbid `MustNewX` calls outside `_test.go` files and `cmd/*/main.go`.

---

## 3. Repository contract

### Position
Interface lives in `domain/<aggregate>/repository.go`. Three methods, exactly this shape (Wild Workouts `internal/trainings/domain/training/repository.go`):

```go
type Repository interface {
    AddTraining(ctx context.Context, tr *Training) error
    GetTraining(ctx context.Context, trainingUUID string, user User) (*Training, error)
    UpdateTraining(
        ctx context.Context,
        trainingUUID string,
        user User,
        updateFn func(ctx context.Context, tr *Training) (*Training, error),
    ) error
}
```

`Add` for new aggregates. `Get` returns the rehydrated aggregate. `Update` takes a **closure** that receives the current state and returns the mutated state — the adapter handles `BEGIN`/`SELECT FOR UPDATE`/`COMMIT`/`ROLLBACK` invisibly.

### Why
TDL: *"My favorite approach is based on a closure passed to the update function"* ([repository-pattern-in-go](https://threedots.tech/post/repository-pattern-in-go/)).

The closure pattern keeps **transaction control inside the adapter** where it belongs (Brandur canon). Handler code never touches `tx.Begin()`; the adapter decides whether to use `FOR UPDATE`, optimistic concurrency, retries on serialization failure, etc. Domain logic runs *inside* the lock window without knowing it.

`Get(..., user)` baking the authorisation predicate into the read prevents the bug class where a handler retrieves a record then forgets to check tenancy/ownership.

### Anti-pattern
- Returning `pgx.Rows`, `sql.Row`, or any driver type — couples consumers to the driver.
- Returning sqlc-generated row structs — same problem at one remove.
- A `Save(agg)` upsert method — hides the "is this a new or existing aggregate?" question that the domain has to answer.
- Handler code calling `tx.Begin()` / `tx.Commit()` directly — transaction control belongs in the adapter.
- A per-use-case method (`CancelTraining(...)`, `ApproveReschedule(...)`) on the repository — pulls business logic out of the aggregate. ([ddd-cqrs-clean-architecture-combined](https://threedots.tech/post/ddd-cqrs-clean-architecture-combined/) explicitly refactors this back to a unified `UpdateTraining`.)
- Schema annotations (`db:"col_name"`, `gorm:"..."`) on domain structs — couples the model to the storage. (See [common-anti-patterns](https://threedots.tech/post/common-anti-patterns-in-go-web-applications/) "Single Model" anti-pattern.)

### How to enforce
- For every file under `internal/*/domain/*/repository.go`, parse the interface; fail if any method returns a type whose package is `pgx`, `pgxpool`, `pgtype`, `database/sql`, `sqlc`, or the local `adapters/db` package.
- Walk `internal/*/app/`; fail on any direct call to `tx.Begin()`, `tx.Commit()`, `tx.Rollback()`, or `pgx.BeginTx`.
- Walk repository interfaces; fail if any method name starts with a business verb (`Cancel*`, `Approve*`, `Reschedule*`) — those are aggregate methods, not repository methods.

---

## 4. Hidden inputs / context smuggling

### Position
`context.Context` carries **transport metadata only**: deadlines, cancellation, request-id, trace span, auth principal (the verified subject of the request). Domain values — `tenant.ID`, `aggregate.ID`, business state — flow as **explicit parameters**.

The active DB transaction is the ONE permitted domain-adjacent ctx value, and it lives in the `adapters` layer only (`pg.TxFromContext(ctx)` is unexported to anything but adapter code).

### Why
TDL does not have a dedicated post on context smuggling, but the implication is throughout: their handler signatures are `Handle(ctx, cmd)` with the command struct carrying every business input. Wild Workouts never reads a tenant ID from `ctx`; the user is a typed parameter on `GetTraining(..., user)`.

The bug class is: two code paths that both compute "current tenant" disagree, and the discrepancy is invisible at the call site. When tenant is an explicit parameter, mismatches are compile errors.

**This is the bug class LeadKart audits kept missing until ADR 0062:** a ctx-GUC tenant transport and an explicit-parameter tenant transport running in parallel, with audits trusting whichever one was inspected first.

### Anti-pattern
- `tenantID := ctx.Value(tenantKey)` inside a command handler — invisible coupling.
- Stashing the current aggregate / current user / current role in ctx — same.
- Multiple ctx-key packages each defining their own "current X" — guarantees drift.

### How to enforce
- Walk every `.go` file outside `internal/common/{httpmw,pg,tenancy}/` and `ports/`; fail on any call to `ctx.Value(...)` whose key is not in a tight allowlist (trace/log/request-id keys only).
- Walk handler `Handle(ctx, cmd)` signatures; require every domain value (tenant ID, actor ID) appear in the `cmd` struct, not in ctx.
- Pick ONE canonical tenant transport. Forbid the others by import path.

---

## 5. Handler shape (orchestrator pattern)

### Position
**No "service" layer.** The command handler IS the use case AND the orchestrator AND the contract. Canonical shape (Wild Workouts `internal/trainings/app/command/cancel_training.go`):

```go
type CancelTraining struct {
    TrainingUUID string
    User         training.User
}

type cancelTrainingHandler struct {
    repo           training.Repository
    userService    UserService     // local interface, defined in services.go
    trainerService TrainerService  // ditto
}

func NewCancelTrainingHandler(
    repo training.Repository,
    userService UserService,
    trainerService TrainerService,
    logger *logrus.Entry,
    metricsClient decorator.MetricsClient,
) decorator.CommandHandler[CancelTraining] { ... }

func (h cancelTrainingHandler) Handle(ctx context.Context, cmd CancelTraining) error { ... }
```

Outbound dependencies are declared as **local interfaces** in `command/services.go` ("accept interfaces, return structs"). The handler is `Handle(ctx, cmd) error` for commands, `Handle(ctx, query) (Result, error)` for queries.

### Why
TDL: *"the application layer can be responsible only for the orchestration of the flow"* ([ddd-cqrs-clean-architecture-combined](https://threedots.tech/post/ddd-cqrs-clean-architecture-combined/)).

A separate "service" layer between handler and domain is dead weight in DDD-shaped code: the domain holds the business rules, the handler orchestrates one use case, and a service layer in between either (a) duplicates the handler, or (b) bleeds business rules out of the aggregate. TDL never has it.

Local interfaces (`UserService` in `command/services.go`) instead of a shared interfaces package keep each handler honest about exactly what it depends on, and let the SQL adapter for `users` evolve without forcing a recompile of `trainings`.

### Anti-pattern
- `internal/<module>/service/` containing business logic — the only `service/` directory in Wild Workouts is the trainings-module composition root, holding `NewApplication` wiring only.
- A `XxxService` interface shared across modules in a top-level `interfaces` package.
- Hard cap on dependency count (e.g. "max 6 deps") — TDL doesn't impose this; the right cap is "as many as the use case needs, declared as local interfaces".
- A handler that calls another handler — use cases compose via the domain or via events, not by mutual call.

### How to enforce
- Forbid the directory name `service/` anywhere except the module composition root.
- For every type matching `*Handler` in `app/command/` or `app/query/`, require exactly one exported method named `Handle` with signature `(context.Context, X) error` or `(context.Context, X) (Y, error)`.
- For every interface declared in `app/command/services.go` (or equivalent), assert no other module imports it — they're handler-local by design.

---

## 6. Testing pyramid

### Position
Per [microservices-test-architecture](https://threedots.tech/post/microservices-test-architecture/) and [database-integration-testing](https://threedots.tech/post/database-integration-testing/):

| Layer | What it tests | How |
|---|---|---|
| Unit (domain) | Business rules — invariants, state machines, VO validation | Pure tests in `domain/<aggregate>/*_test.go` |
| Unit (handler) | Orchestration — calls repo, emits events, error paths | Tests in `app/command/*_test.go` against per-aggregate `FakeRepository` |
| Integration (adapter) | SQL contract — RLS fires, JSONB round-trip, constraint translation, outbox-row write | `_repository_pg_test.go` against real Postgres via testcontainers |
| Component | Full service with real infra, mocked external services | `service/component_test.go` — happy path only |
| E2E | Critical cross-service contracts | Few; smoke-test depth only |

**Fakes, not mocks.** Every aggregate ships a `<aggregate>test/fake_repository.go`. The fake passes the **same contract test suite** as the SQL adapter, proving behavioural equivalence.

### Why
TDL: *"All Pub/Sub implementations are passing the same test suite"* ([database-integration-testing](https://threedots.tech/post/database-integration-testing/)). Mocks freeze a snapshot of what the handler thought the repo would do; fakes prove the handler will work against the real adapter because both pass the same tests.

Speed budget: *"Tests must run locally in under 10 seconds; CI tests in under 1 minute."* That budget kills any "test everything through Postgres" temptation.

The SQL-contract vs business-rule split is load-bearing: SQL tests assert what only the SQL adapter can verify (RLS firing, SQLSTATE 23505 translation, partial-index behaviour, EXPLAIN under RLS). Business rules are tested at the domain/handler layer where they actually live.

### Anti-pattern
- Mocks generated from interfaces in handler tests — freeze a snapshot, can't catch contract drift.
- Integration-testing business rules (testing "training can't be rescheduled twice" through the SQL adapter) — slow, redundant with domain tests.
- Domain-testing through the SQL layer ("I'll just round-trip it through Postgres to be sure") — same problem.
- Cleanup-based isolation (truncate-between-tests) — slow, race-prone. TDL prefers unique UUIDs per test for parallel isolation.
- `t.Parallel()` plus shared-fixture truncation in the same suite — LeadKart already has `TestArch_TruncateAllImpliesSerial`. Keep it.
- `sleep()` for synchronization — TDL: *"Never use sleep(); synchronize with channels or sync.WaitGroup."*

### How to enforce
- For every `domain/<aggregate>/repository.go`, require a sibling `<aggregate>test/fake_repository.go` with `var _ <aggregate>.Repository = (*FakeRepository)(nil)`.
- Forbid mock-generation tools (mockery, gomock) under `internal/` — adapters and fakes only.
- For every `_repository_pg_test.go`, fail if the test body doesn't reference at least one of: `RLS`, `SQLSTATE`, `pgconn.PgError`, `outbox`, `EXPLAIN`, `index`. (Heuristic — flags tests doing pure round-trips that belong at the fake layer.)
- Forbid `time.Sleep` in `_test.go` files except behind a `//nolint:test_sleep` comment with justification.

---

## 7. Concurrency in domain

### Position
The domain layer is concurrency-free. No `sync.Mutex`, no `sync.RWMutex`, no `chan`, no `go` statements, no `sync/atomic`. Concurrency lives only in:
- `adapters/` (DB connection pool, broker producers/consumers)
- `ports/` (HTTP server, subscriber dispatch)
- `cmd/api/main.go` (signal handling, server lifecycle)

### Why
Bryan Mills, "Rethinking Concurrency Patterns" (GopherCon 2018): concurrency is a property of the I/O boundary, not of the business model. Domain code that holds locks is doing I/O coordination in the wrong place — the lock should be in the adapter (DB row lock via `SELECT FOR UPDATE`, or optimistic concurrency via version column), not in the aggregate.

If you find yourself reaching for a mutex in a domain method, the question is "what concurrent invariant am I protecting?" — and the answer is almost always "the database row's version", which `UpdateTraining`'s closure-on-tx pattern already handles.

### Anti-pattern
- `sync.Mutex` field on an aggregate.
- A goroutine spawned from a domain method.
- A channel field on an aggregate.
- A `sync.Once` in a domain ctor (use the rehydration-vs-ctor split instead).

### How to enforce
- Walk every `.go` file under `internal/*/domain/`; fail on any import of `sync` or `sync/atomic`, on any `go ` statement, on any `chan` type declaration.

---

## 8. Cross-aggregate communication

### Position
- **Within a bounded context, between aggregates:** outbox event + subscriber. Even in-process.
- **Across bounded contexts:** outbox event + Watermill forwarder + broker subscriber. Mandatory.
- **Synchronous gRPC** is allowed for **query** flows (read-only, idempotent, latency-sensitive) but the strong preference is async events for state changes.

Aggregate transactional boundary is **one aggregate per command**. A command that needs to change two aggregates becomes: command-handler-A mutates aggregate-A and writes event-A; subscriber-B handles event-A and mutates aggregate-B in its own transaction.

### Why
TDL: *"Stay away from transactions that span more than one service, except when there's no other way... Your microservice boundaries are likely wrong if you consider using a distributed transaction"* ([distributed-transactions-in-go](https://threedots.tech/post/distributed-transactions-in-go/)).

The outbox pattern (Watermill `forwarder`) makes event-with-state atomic: the event row lands in the same Postgres transaction as the aggregate update; the background forwarder publishes to the broker afterwards. *"Save the event in the same database... within the same transaction. Then, asynchronously publish it to the Pub/Sub."*

Eventual consistency is the rule, not the exception: *"Not all operations need to be strongly consistent, even if it initially seems like it."*

Cross-aggregate direct calls (handler-A calling handler-B in the same tx) are the path to the distributed monolith — even in a process. They couple deployment lifecycle ("if I touch handler-B I have to retest handler-A") and forbid the eventual extraction of B into its own service.

### Anti-pattern
- One command handler calling another command handler.
- One command handler calling another module's aggregate repository directly.
- An "in-process event bus" that fans out synchronously inside the same DB tx (looks like events, behaves like coupling).
- A query handler reading from another module's domain types directly — read models / DTOs only across the boundary.
- A "saga coordinator" in the domain — coordination belongs in a subscriber, not in the aggregate.

### How to enforce
- For every `internal/X/app/`, fail on any import of `internal/Y/domain/` or `internal/Y/app/` for X ≠ Y.
- For every `internal/X/adapters/`, fail on any import of `internal/Y/domain/` for X ≠ Y (DTOs from `internal/common/` or `api/` are allowed).
- For every outbox publish call, require it to occur inside the same `UpdateXxx` closure as the aggregate mutation (i.e. inside `pg.TxFromContext(ctx)`).
- Forbid `internal/<module>/app/*` from importing other modules entirely; subscribers in `internal/<module>/ports/subscribers/` are the only path.

---

## 9. Dependency injection

### Position
**No DI container.** Manual constructor wiring at the composition root. Each module has a `service/service.go` (the only legitimate `service/` directory) holding `NewApplication(ctx) (app.Application, cleanup func())`. The HTTP entry point is `cmd/api/main.go` (or per-module `main.go` in Wild Workouts), Mat Ryer's `NewServer(cfg, log, app, ...)` big-positional-ctor pattern.

### Why
TDL: *"In their projects, they don't use a library for dependency injection but do it by hand. If you really need to use any, they recommend wire because it's code generation based."* DI containers (Uber fx, dig, samber/do) introduce runtime resolution failures that the compiler can't catch and obscure the actual dependency graph — exactly the visibility a "manual wiring" composition root preserves.

Mat Ryer's 2024 essay: a constructor with many positional arguments is fine when it's called once at startup; the compile-time signature *is* the documentation of what the server needs.

### Anti-pattern
- An IoC container (`samber/do`, `uber/fx`, `uber/dig`) holding the service graph at runtime.
- A `service registry` package where types register themselves at init time.
- Functional options on application services (`NewServer(WithRepo(r), WithBus(b))`) — TDL's options-pattern stance is "library code only; app code prefers an explicit options struct or positional ctor".
- Wire-generated code outside `cmd/*/wire_gen.go` — wire is acceptable but limit it to the composition root; don't sprinkle generated code into `app/`.

### How to enforce
- Forbid imports of `go.uber.org/fx`, `go.uber.org/dig`, `samber/do` anywhere in the repo.
- Require every module to have exactly one `service/service.go` exposing `NewApplication(...)` (LeadKart deviates here — composition is in `cmd/api/main.go`; doc it OR fix it).
- Forbid functional options on constructors in `app/` and `adapters/` (allow only on packages tagged "public library" — currently none).

---

## 10. Error handling

### Position
- **Domain errors are sentinel values:** `var ErrHourNotAvailable = errors.New("...")`. Handlers compare via `errors.Is(err, domain.ErrHourNotAvailable)`.
- **Typed errors** (struct types implementing `error`) for cases that carry data: `NotFoundError{TrainingUUID: ...}`. Wild Workouts uses this exact shape.
- **Wrap with `fmt.Errorf("...: %w", err)`** at layer boundaries to preserve `errors.Is`/`errors.As` chains.
- **Panic only at init time** and in tests. `MustNewX` is permitted in `main.go` and `_test.go` exclusively.

### Why
TDL's repository implementations use a `NotFoundError` struct (visible in `training.Repository`); their pubsub adapters use sentinel errors throughout Watermill. The split is utilitarian: sentinels for type-only signaling ("does this error mean X?"), typed for payload-carrying ("not found — which UUID?").

`panic` in a request path crashes the process for a single bad request; in a long-running server that means dropping every concurrent in-flight request too. Init-time `panic` is fine because there's nothing to drop yet.

### Anti-pattern
- `if err.Error() == "not found"` string comparison.
- Returning `nil, nil` for "not found" — forces every caller to check both.
- A bare `panic(err)` in a handler.
- A typed error whose only field is the message (use a sentinel instead).
- A god-error type with a `Code` field and a switch — that's reinventing `errors.Is`.

### How to enforce
- Forbid `panic(` in any `.go` file outside `_test.go`, `cmd/*/main.go`, and explicitly-allowlisted `MustX` constructors.
- Forbid string comparison on error values (`err.Error() ==`).
- For every `domain/<aggregate>/`, require a `errors.go` (or sentinel declarations in `<aggregate>.go`) declaring at minimum a `ErrNotFound`-equivalent.

---

## 11. Validation

### Position
**Two-layer validation:**
- **Transport layer (ports):** structural validation — fields present, types parseable, lengths sane. `go-playground/validator` tags on DTO structs are acceptable here (and only here).
- **Domain layer:** business invariants — "training is in the future", "hour is available", "user has balance". Enforced in aggregate ctor and mutator methods. **No tags. Pure Go.**

The command struct is plain data (`CancelTraining{TrainingUUID, User}`); it has neither validator tags nor business validation. Translation: DTO → command → domain.

### Why
TDL: *"Validation is just one part of the business logic you find in most web applications"* but placing it in HTTP handlers prevents reuse across APIs ([common-anti-patterns](https://threedots.tech/post/common-anti-patterns-in-go-web-applications/)).

The bug class: business validation in struct tags means the business rule is invisible to anyone reading the domain — and silently absent on any code path that bypasses the tag-validating adapter (events, internal admin tools, migrations).

Structural validation at the DTO layer is fine because it's about "can this request even be parsed", not "is this a valid business operation".

### Anti-pattern
- `validate:"required"` tag on a domain entity field.
- A single struct used as both API DTO and domain entity (the "Single Model" anti-pattern from common-anti-patterns).
- Business validation in HTTP handlers (`if cmd.Email == ""` checks before delegating to the use case) — duplicates domain logic and lets it drift.
- A validator-tag rule that encodes a business rule a domain method should own (`validate:"future_time"` instead of `training.ScheduleTraining(when)` checking `when.After(time.Now())`).

### How to enforce
- Walk `internal/*/domain/`; fail on any struct tag of the form `validate:"..."`.
- Walk `internal/*/app/command/` and `internal/*/app/query/`; fail on `validate:` tags on Command/Query structs.
- Allow `validate:` tags only under `internal/*/ports/` and only on DTO types (heuristic: name ends in `Dto`, `Request`, or `Response`).

---

## 12. Composition root

### Position
Two-tier composition:
- **Module composition root:** `internal/<module>/service/service.go` exports `NewApplication(ctx) (app.Application, cleanup func())`. Builds adapters, builds handlers, wires them into the `Application{Commands, Queries}` facade. Returns a `cleanup` closure for resource teardown.
- **Process composition root:** `cmd/api/main.go` calls each module's `NewApplication`, registers their `ports.AddRoutes`, starts the HTTP server. Mat Ryer 2024 `NewServer(cfg, log, deps...)` pattern. Zero business logic; signal handling and lifecycle only.

The Wild Workouts trainings example is exactly this:
```go
func main() {
    logs.Init()
    ctx := context.Background()
    app, cleanup := service.NewApplication(ctx)
    defer cleanup()
    server.RunHTTPServer(func(router chi.Router) http.Handler {
        return ports.HandlerFromMux(ports.NewHttpServer(app), router)
    })
}
```

### Why
The composition root is the **one place** where the entire dependency graph is visible. Manual wiring there gives you compile-time proof that every handler has every dependency satisfied, with stack-traceable failures (vs. runtime resolution failures from a DI container).

Splitting per-module `service/` + process-level `main.go` lets each module own its construction order (its own DB pool, its own outbox subscriber set) while keeping the process-level `main.go` thin and free of cross-module knowledge.

### Anti-pattern
- A `init()` function registering a handler with a global registry.
- Module wiring code in `cmd/api/main.go` itself ("the main.go that grew teeth").
- A shared `internal/composition/` package importing every module's adapters — couples modules at composition time.
- A factory that takes a `Config` blob and reaches into it for every nested value — pass concrete typed parameters per module.

### How to enforce
- Forbid `init()` functions outside `cmd/*/main.go` and `_test.go`. (Allow narrow exceptions for tag-registration with a `//nolint:init` justification.)
- Require every `internal/<module>/` to expose exactly one `service.NewApplication` (or equivalent named composition entry point). **LeadKart currently doesn't have per-module service/ — wiring is in cmd/api/main.go. Either follow TDL or document the deviation with rationale.**
- Forbid `cmd/api/main.go` from importing any `internal/<module>/adapters/` directly — it talks to modules only through their `service/` and `ports/` packages.

---

## Cross-cutting principles that don't fit a single domain

### "Make invalid states unrepresentable"
TDL on enums ([safer-enums-in-go](https://threedots.tech/post/safer-enums-in-go/)): structs with unexported fields + package-level sentinels. *"The only invalid role you can construct is the empty one: `Role{}`."* Apply this to every value object — `email.Address`, `tenant.Slug`, `money.Amount`. No primitive obsession; no `string` typedefs that accept any string.

### "Duplication < wrong abstraction"
TDL on DRY ([things-to-know-about-dry](https://threedots.tech/post/things-to-know-about-dry/)): *"DRY is better applied to behaviors, not data."* The DTO ↔ domain ↔ DB-row triplet looks redundant; it is the architecture working. A shared "User" struct used across all three layers is the "Single Model" anti-pattern.

### "Iteration speed is the quality metric"
TDL: *"The best indicator of codebase quality is iteration speed."* This is why mocks (which freeze) lose to fakes (which co-evolve with the contract); why ctx-smuggled state (which is invisible) loses to explicit params (which the compiler reasons about); why distributed transactions (which require coordination to change) lose to outbox events (which let each side evolve independently).

### "Architecture is reversible"
TDL: *"How you deploy them (as a modular monolith or microservices) is an implementation detail."* The whole point of the layering rules is that extracting a module to its own service should be a `git mv` and a swap of `intraprocess` for `gRPC`/events — not a six-month rewrite. Every arch test that prevents cross-module domain imports is paying premium on this option.

---

## References

- [threedots.tech / ddd-lite-in-go-introduction](https://threedots.tech/post/ddd-lite-in-go-introduction/) — Aggregate ctor pattern, behaviour-only methods, rejection of setters, `Hour.ScheduleTraining()` example.
- [threedots.tech / repository-pattern-in-go](https://threedots.tech/post/repository-pattern-in-go/) — Interface lives with domain; closure-based `Update`; no driver types in return values; three implementations passing one contract.
- [threedots.tech / ddd-cqrs-clean-architecture-combined](https://threedots.tech/post/ddd-cqrs-clean-architecture-combined/) — Application orchestrates only; no per-use-case repo methods; unified `UpdateTraining` shape.
- [threedots.tech / introducing-clean-architecture](https://threedots.tech/post/introducing-clean-architecture/) — Four layers; "outer to inner" import rule; `go-cleanarch` linter recommendation.
- [threedots.tech / microservices-or-monolith-its-detail](https://threedots.tech/post/microservices-or-monolith-its-detail/) — Architectural reversibility via clean module boundaries; intraprocess vs gRPC swap.
- [threedots.tech / basic-cqrs-in-go](https://threedots.tech/post/basic-cqrs-in-go/) — Command/CommandHandler shape; `Application{Commands, Queries}` facade; HTTP wiring example.
- [threedots.tech / common-anti-patterns-in-go-web-applications](https://threedots.tech/post/common-anti-patterns-in-go-web-applications/) — Distributed monolith, single model, magic libraries, omitted tags, mixed logic, over-simplification, schema-first, CRUD-first, directory-first.
- [threedots.tech / database-integration-testing](https://threedots.tech/post/database-integration-testing/) — Fakes-over-mocks; one contract, multiple implementations; speed budget; parallel + unique-data isolation; "never sleep()".
- [threedots.tech / microservices-test-architecture](https://threedots.tech/post/microservices-test-architecture/) — Test pyramid layering; component-test definition; minimal E2E.
- [threedots.tech / distributed-transactions-in-go](https://threedots.tech/post/distributed-transactions-in-go/) — "If you need distributed transactions, your boundaries are wrong"; outbox + eventual consistency as the answer.
- [threedots.tech / safer-enums-in-go](https://threedots.tech/post/safer-enums-in-go/) — Struct + unexported-field + package-level sentinels; `Role{}` as the only constructible invalid value.
- [threedots.tech / things-to-know-about-dry](https://threedots.tech/post/things-to-know-about-dry/) — DRY is for behaviour, not data; layer-spanning struct sharing is an anti-pattern.
- [threedots.tech / robust-grpc-google-cloud-run](https://threedots.tech/post/robust-grpc-google-cloud-run/) — When to use gRPC (sync, strict contracts); even with robust contracts, async events are still preferred for state changes.
- [threedots.tech / go-with-the-domain/](https://threedots.tech/go-with-the-domain/) — Book overview; "learn to solve problems, not apply patterns".
- [GitHub / ThreeDotsLabs/wild-workouts-go-ddd-example](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example) — Canonical reference repo:
  - `internal/trainings/` layout (`adapters/`, `app/`, `domain/training/`, `ports/`, `service/`, `main.go`)
  - `internal/trainings/domain/training/repository.go` — canonical three-method interface with closure-based `UpdateTraining`
  - `internal/trainings/app/command/services.go` — local interfaces (`UserService`, `TrainerService`) co-located with the handler that consumes them
  - `internal/trainings/app/app.go` — `Application{Commands, Queries}` facade
  - `internal/trainings/service/service.go` — composition root (`NewApplication(ctx)` + `cleanup`)
  - `internal/trainings/main.go` — Mat Ryer-shaped thin entry point
- [watermill.io / advanced/forwarder](https://watermill.io/advanced/forwarder/) — Outbox pattern producer/consumer split; atomicity via DB-local tx; background daemon forwards to broker.
- Mat Ryer, "How I write HTTP services in Go after 13 years" (2024) — `NewServer(cfg, log, deps...)` big-ctor pattern; manual wiring at composition root.
