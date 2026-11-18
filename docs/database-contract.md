# MySQL schema contract

MySQL database, table, column, user and migration definitions are owned by `finsights-coredb`. This file records the contracts the backend services expect so runtime SQL remains aligned with those managed schemas.

All timestamps are UTC. Use parameterized SQL and decimal/integer types for market values. Each service receives a separate database/schema and least-privilege DB account through its own `*_MYSQL_DSN` secret.

## Auth schema

- `accounts`: `id` (numeric primary key), `email` (unique), `password_hash`, `created_at`.
- `outbox_events`: `id` (ordered primary key), `event_id` (unique), `event_type`, `payload` (JSON), `created_at`, nullable `published_at`.

The account row and `account.created` outbox row must commit in one transaction. The relay publishes with RabbitMQ message persistence and marks the event published only after broker confirmation. Delivery is at least once; `event_id` is the deduplication key.

## User schema

- `users`: `account_id` (unique; auth account identifier), `display_name`, `created_at`, `updated_at`.
- `watchlist_symbols`: unique key over (`account_id`, `symbol`), plus `created_at`.

The `account_id` comes from Auth and is provisioned by the RabbitMQ consumer. Repeated `account.created` events must not create a second profile.

## Market schema

- `instruments`: `symbol` (primary key), `name`, `short_name`, `sector`, `exchange`, `market_cap`.
- `stock_quotes`: `id`, `symbol`, `price`, `change_percent`, `volume`, `quoted_at`; index (`symbol`, `quoted_at`).
- `stock_candles`: `id`, `symbol`, `candle_at`, `open_price`, `high_price`, `low_price`, `close_price`, `volume`; unique (`symbol`, `candle_at`).
- `market_indices`: `symbol` (primary key), `name`, `value`, `change_value`, `change_percent`, `exchange`, `volume`, `quoted_at`.
- `news_articles`: `id`, `slug` (unique), `title`, `description`, `category`, `image_url`, `source_name`, `source_url`, `published_at`, `body_markdown`.
- `news_article_symbols`: unique (`article_id`, `symbol`), linking news to instruments.

The current Vietstock DataFeed collector writes market quote/index snapshots and daily rows through the Node 2 market account. It retains provider codes and observation timestamps so the UI can distinguish fresh data from stale data. New feeds should write normalized source data to their domain database and keep this API-facing contract stable until MarketService is extended.

## AI chat history schema (MySQL Node 1)

`finsights_ai` is provisioned by `finsights-coredb/mysql-node-1`. Its schema is unchanged. `chat_sessions` stores the stable UUID exposed to clients, the authenticated account identifier in `user_uid`, session title/provider/status and timestamps. `chat_messages` stores ordered user/assistant turns, completion status, model name, token counts and a non-sensitive error code. Reads and session closure always filter by both `session_uid` and the user identifier derived from the verified JWT. `user_uid` is the current numeric Auth account ID stored as a string; this database intentionally has no cross-database foreign key.

The existing Auth, User, and Market service schemas are provisioned on MySQL Node 2. Their service contracts are unchanged; only endpoint placement and TLS CA selection change.

## AI knowledge schema (Oracle Autonomous Database)

`services/ai/sql/001_oracle_knowledge_chunks.sql` creates `ai_knowledge_chunks` in a dedicated least-privilege Oracle schema. The service stores normalized news chunks, source URL/title/published time, content hashes, metadata and `VECTOR(1024,FLOAT32)` embeddings. Vector width must match the configured Voyage model output. The initial implementation performs exact cosine-distance scans; index creation is deferred until measured corpus size and latency justify it. Chat transcripts and user IDs are not copied into this vector table.

## RabbitMQ event contract

Exchange: `finsights.events` (durable topic). Routing key: `account.created`. Payload:

```json
{
  "event_id": "unique-event-id",
  "account_id": 123,
  "display_name": "Example"
}
```

The user consumer declares a durable queue and retry/dead-letter queues. It must acknowledge only after profile persistence succeeds.
