# MySQL schema contract

Migrations are intentionally not included here. The local infrastructure documentation assigns schema and migration ownership to `finsights-core-db`, and the user requested waiting for that repository. This file records only the tables and columns the service code expects so the migration can be implemented in the owning repo.

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

No market-data source or ingestion worker is selected yet. The schema must retain source attribution and quote timestamps so the UI can distinguish fresh data from stale data when ingestion is added.

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
