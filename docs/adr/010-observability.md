# ADR-010: Observability with OpenTelemetry

## Status

Accepted: 2026-07-07

## Context

Through Week 7 the ledger had two network surfaces (REST on chi, gRPC) and two
observability signals that did not talk to each other: native Prometheus metrics
on a loopback `/metrics` server (ADR-004), and structured slog lines with a chi
request id but no trace context. ADR-009 deliberately left a clean interceptor
seam and deferred tracing to this week.

Week 8 closes that gap. The definition of done: a single transaction post
produces one distributed trace spanning HTTP or gRPC, the domain service, and
the SQL round-trips, with every log line for that request carrying the same
`trace_id`. Traces run to Jaeger locally and to Honeycomb's free tier remotely.

The build plan flags this as a heavy week with an OpenTelemetry-config rabbit
hole risk, and says to time-box to eight hours and ship slog-correlation plus
Prometheus RED if the remote export fights back. Several choices had no obvious
default, and one early instinct (route every metric through OpenTelemetry) turned
out to be the wrong call for a service that already has metrics tied to alerting
baselines.

## Decision

### Hybrid metrics: native domain collectors, OTel meter for RED

The four existing domain collectors stay exactly as they are, native
`client_golang` on the dedicated registry, with their current names:
`transaction_post_duration_seconds`, `transaction_post_serialization_retries_total`,
`transaction_idempotency_replays_total`, `transaction_idempotency_conflicts_total`.
These are SLO metrics tied to the Week 4 latency baselines. Renaming them is how
you silently break an alert and find out during an incident, so they do not move.

The Rate, Errors, Duration signals for the two transports come through the
OpenTelemetry metric API instead: `otelhttp` emits `http.server.request.duration`
and `otelgrpc` emits `rpc.server.duration`, both with status attributes that give
rate and errors for free. Those flow through an OTel `MeterProvider` whose
Prometheus exporter is registered into the same registry the native collectors
use, so `/metrics` stays a single loopback endpoint. Runtime instrumentation is
not enabled on the OTel side: the existing `NewGoCollector` and
`NewProcessCollector` remain the only source of `go_*` and `process_*`, avoiding a
double registration.

The rejected alternative was routing all metrics through OpenTelemetry. It buys a
single metric API, but the OTel Prometheus exporter mangles output (unit
suffixes, `otel_scope_*` labels, a `target_info` series) and would have renamed
the alert-bearing metrics for no operational gain. The hybrid keeps the
invariant-critical metrics stable and still gets unified RED. A one-API purity win
was not worth breaking working alerts in a payment service.

### Tracing exporter selected by environment, loud when misconfigured

One tracer setup, three modes chosen at startup:

- `OTEL_EXPORTER_OTLP_ENDPOINT` set: an OTLP/HTTP exporter (`otlptracehttp`),
  which reads the standard OTLP environment variables for endpoint and headers.
  This is the only mode used in production and against Jaeger.
- Endpoint unset but `OTEL_TRACES_EXPORTER=console`: a stdout exporter, for
  eyeballing spans in local development without running a collector.
- Neither set: a no-op tracer and one `warn` log line at startup.

The stdout exporter is opt-in, not the silent default. On the VPS a missing or
misconfigured endpoint must be a loud no-op, not a flood of span JSON dumped into
the same stdout stream the logs are correlated on. OTLP/HTTP over gRPC because it
is the lower-ops choice, firewall-friendly, and Honeycomb-native; the app already
runs a gRPC server, so an OTLP/gRPC exporter would not be exotic, but there is no
payoff over HTTP here.

### Head sampling, and the noise paths excluded

The sampler is `ParentBased` with the root sampler read from the standard
`OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` variables, defaulting to
always-on for local development. Production sets a ratio (documented as
`parentbased_traceidratio` at `0.25`) because the demo seeder writes about 285
transactions every four hours, each fanning out to per-query DB spans under
SERIALIZABLE retries, and Honeycomb's free tier is twenty million events a month.
Always-on plus that write volume burns the budget on demo noise.

Two classes of span are dropped regardless of sampler: health and metrics
requests are filtered out of HTTP tracing (`otelhttp` `WithFilter` on `/healthz`,
and the loopback metrics server is not wrapped at all), so traces are real
request work and not a wall of health checks.

### Spans at four layers, no money or PII in attributes

- HTTP: the chi router is wrapped with `otelhttp`; a small middleware upgrades the
  span name to the matched chi route pattern (not the raw path) so URL cardinality
  cannot explode the trace backend.
- gRPC: `otelgrpc.NewServerHandler` is added as a `StatsHandler` alongside the
  existing interceptor chain from ADR-009, which stays unchanged.
- Domain: `TransactionService.Post` and the key `AccountService` reads open manual
  spans. Failure paths call `RecordError` and set an error status, and the
  double-entry invariant rejection from `Transaction.Validate()` is recorded as a
  span error, because that is the event a reviewer actually wants to find.
- Database: `otelpgx` is the pool query tracer, one span per SQL statement.

Span attributes are restricted to safe identifiers: `tenant_id`,
`transaction_id`, posting count, and whether an idempotency key was present. No
amounts, no balances, no account identifiers as values, and `otelpgx` runs with
query arguments off so bind values never leave the process. Honeycomb is a US SaaS
processor, and shipping customer financial metadata to a third party is a decision
that needs to be made on purpose, not leaked through a default. For a demo and
portfolio service the rule is simply: identifiers and counts, never money.

### Logs correlate through a wrapping slog handler

A thin `slog.Handler` wraps the existing JSON handler. On each record it reads the
active span context and, when valid, injects `trace_id`, `span_id`, and a
`sampled` flag, so every line logged with a request context correlates to its
trace. It delegates `WithAttrs` and `WithGroup` to the inner handler and rewraps,
so grouped and pre-attributed loggers keep working. This is a dozen lines and no
new dependency.

The rejected alternative was the `otelslog` bridge, which turns log records into
OTel log records shipped over OTLP. That is remote log export, a signal this week
does not ship and the plan does not scope. The goal is correlation, not a third
pipeline.

### Service version from build info, not a hand-set string

The trace `Resource` carries `service.name`, `service.version`, and
`deployment.environment`. The version is read from `runtime/debug.BuildInfo`
(the VCS revision the binary was built from) rather than a hand-maintained
constant, so every trace can be attributed to a release without a manual bump. The
CI pipeline builds from a checked-out commit, so the revision is populated.

## Consequences

### Positive

- One transaction post yields one linked trace across transport, service, and SQL,
  and its log lines carry the same `trace_id`: the definition of done, verifiable
  in Jaeger locally and Honeycomb remotely.
- The alert-bearing domain metrics keep their exact names, so nothing built on the
  Week 4 baselines breaks, while RED for both transports arrives for free through
  the instrumentation libraries.
- Tracing is off by default with no endpoint, so local development and the VPS run
  clean without a collector, and a misconfiguration is a loud no-op rather than a
  silent log flood.
- No money, balances, or bind values leave the process, so enabling a third-party
  backend does not quietly export financial data.

### Negative

- Two metric APIs now coexist: native `client_golang` for the domain metrics and
  the OTel meter for RED. That is a deliberate trade of purity for stability, and
  it means a contributor sees both styles in the codebase.
- OpenTelemetry adds several dependencies (`otel/sdk`, the OTLP/HTTP and stdout
  trace exporters, the Prometheus metric exporter, `otelgrpc`, `otelpgx`) and a
  startup path that must be shut down cleanly within the existing graceful-shutdown
  budget so a batch exporter flush cannot hang termination.
- Span attributes are deliberately thin. Anyone wanting richer trace data must
  first decide what is safe to send to a third party, which is friction by design.

## Alternatives considered

- **All metrics through OpenTelemetry**: rejected. It renames alert-bearing SLO
  metrics and adds `target_info` and `otel_scope_*` noise for a single-API win
  that does not pay for breaking working alerts. Hybrid keeps the domain metrics
  native and still unifies RED.
- **stdout as the silent no-endpoint default**: rejected. A misconfigured endpoint
  in production would dump span JSON into the correlated log stream. Console export
  is opt-in; the real default is a loud no-op.
- **OTLP over gRPC (port 4317)**: not chosen. HTTP/protobuf is the lower-ops,
  firewall-friendly path and Honeycomb-native; running gRPC elsewhere does not
  make it the better exporter here.
- **Always-on sampling**: rejected for production. The seeder plus per-query DB
  spans would exhaust the Honeycomb free tier on demo traffic; a parent-based ratio
  plus dropping health and metrics paths keeps the signal and the budget.
- **`otelslog` log bridge**: deferred. It ships logs over OTLP, a signal this week
  does not export. A wrapping handler gives the correlation the DoD asks for
  without a new pipeline.
- **`otelchi` for route-named HTTP spans**: not adopted. A few lines of chi
  middleware over the already-present `otelhttp` set the route pattern without
  another dependency.
