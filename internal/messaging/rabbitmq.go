package messaging

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/rabbitmq/amqp091-go"
)

type Publisher struct {
	connection    *amqp091.Connection
	channel       *amqp091.Channel
	exchange      string
	confirmations <-chan amqp091.Confirmation
	returns       <-chan amqp091.Return
}

func NewPublisher(url, exchange string) (*Publisher, error) {
	connection, err := amqp091.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("connect RabbitMQ: %w", err)
	}
	channel, err := connection.Channel()
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("open RabbitMQ channel: %w", err)
	}
	if err := channel.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		_ = channel.Close()
		_ = connection.Close()
		return nil, fmt.Errorf("declare RabbitMQ exchange: %w", err)
	}
	if err := channel.Confirm(false); err != nil {
		_ = channel.Close()
		_ = connection.Close()
		return nil, fmt.Errorf("enable RabbitMQ publisher confirms: %w", err)
	}
	confirmations := channel.NotifyPublish(make(chan amqp091.Confirmation, 1))
	returns := channel.NotifyReturn(make(chan amqp091.Return, 1))
	return &Publisher{connection: connection, channel: channel, exchange: exchange, confirmations: confirmations, returns: returns}, nil
}

func (p *Publisher) Close() {
	_ = p.channel.Close()
	_ = p.connection.Close()
}

func (p *Publisher) Publish(ctx context.Context, route string, eventID string, body []byte) error {
	if err := p.channel.PublishWithContext(ctx, p.exchange, route, true, false, amqp091.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp091.Persistent,
		MessageId:    eventID,
		Timestamp:    time.Now().UTC(),
		Body:         body,
	}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case confirmation, ok := <-p.confirmations:
		if !ok {
			return fmt.Errorf("RabbitMQ publisher confirmation channel closed")
		}
		if !confirmation.Ack {
			return fmt.Errorf("RabbitMQ did not confirm event %s", eventID)
		}
		select {
		case returned, ok := <-p.returns:
			if ok && returned.MessageId == eventID {
				return fmt.Errorf("RabbitMQ could not route event %s", eventID)
			}
		default:
		}
		return nil
	}
}

type outboxRow struct {
	ID        int64
	EventType string
	Payload   []byte
}

// RunOutboxRelay is at-least-once: a crash after publish and before the SQL
// commit can redeliver an event. Consumers must deduplicate by event_id.
func RunOutboxRelay(ctx context.Context, db *sql.DB, url, exchange string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var publisher *Publisher
	for {
		if publisher == nil {
			var err error
			publisher, err = NewPublisher(url, exchange)
			if err != nil {
				slog.Error("outbox relay could not connect to RabbitMQ", "error", err)
			}
		}
		if publisher != nil {
			if err := publishOutboxBatch(ctx, db, publisher); err != nil {
				slog.Error("outbox relay failed", "error", err)
				publisher.Close()
				publisher = nil
			}
		}
		select {
		case <-ctx.Done():
			if publisher != nil {
				publisher.Close()
			}
			return
		case <-ticker.C:
		}
	}
}

func publishOutboxBatch(ctx context.Context, db *sql.DB, publisher *Publisher) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id, event_type, payload FROM outbox_events WHERE published_at IS NULL ORDER BY id LIMIT 25 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	var batch []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.ID, &row.EventType, &row.Payload); err != nil {
			_ = rows.Close()
			return err
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, row := range batch {
		var envelope struct {
			EventID string `json:"event_id"`
		}
		if err := json.Unmarshal(row.Payload, &envelope); err != nil {
			return fmt.Errorf("decode outbox event %d: %w", row.ID, err)
		}
		if err := publisher.Publish(ctx, row.EventType, envelope.EventID, row.Payload); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_events SET published_at = UTC_TIMESTAMP(3) WHERE id = ?`, row.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
