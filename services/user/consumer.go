package user

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rabbitmq/amqp091-go"
)

type accountCreated struct {
	EventID     string `json:"event_id"`
	AccountID   int64  `json:"account_id"`
	DisplayName string `json:"display_name"`
}

func RunAccountCreatedConsumer(ctx context.Context, url, exchange, queue string, db *sql.DB) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := consumeAccountCreated(ctx, url, exchange, queue, db); err != nil && ctx.Err() == nil {
			slog.Error("account.created consumer stopped", "error", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func consumeAccountCreated(ctx context.Context, url, exchange, queue string, db *sql.DB) error {
	connection, err := amqp091.Dial(url)
	if err != nil {
		return fmt.Errorf("connect RabbitMQ: %w", err)
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		return err
	}
	defer channel.Close()
	if err := channel.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	retryExchange := exchange + ".retry"
	deadExchange := exchange + ".dead"
	if err := channel.ExchangeDeclare(retryExchange, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	if err := channel.ExchangeDeclare(deadExchange, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	args := amqp091.Table{"x-dead-letter-exchange": retryExchange, "x-dead-letter-routing-key": queue + ".retry"}
	if _, err := channel.QueueDeclare(queue, true, false, false, false, args); err != nil {
		return err
	}
	if err := channel.QueueBind(queue, "account.created", exchange, false, nil); err != nil {
		return err
	}
	if _, err := channel.QueueDeclare(queue+".retry", true, false, false, false, amqp091.Table{
		"x-message-ttl":             int32(5000),
		"x-dead-letter-exchange":    exchange,
		"x-dead-letter-routing-key": "account.created",
	}); err != nil {
		return err
	}
	if err := channel.QueueBind(queue+".retry", queue+".retry", retryExchange, false, nil); err != nil {
		return err
	}
	deadQueue := queue + ".dead"
	if _, err := channel.QueueDeclare(deadQueue, true, false, false, false, nil); err != nil {
		return err
	}
	if err := channel.QueueBind(deadQueue, deadQueue, deadExchange, false, nil); err != nil {
		return err
	}
	if err := channel.Qos(10, 0, false); err != nil {
		return err
	}
	if err := channel.Confirm(false); err != nil {
		return fmt.Errorf("enable RabbitMQ dead-letter publisher confirms: %w", err)
	}
	confirmations := channel.NotifyPublish(make(chan amqp091.Confirmation, 1))
	returns := channel.NotifyReturn(make(chan amqp091.Return, 1))
	deliveries, err := channel.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("RabbitMQ delivery channel closed")
			}
			if err := handleAccountCreated(ctx, db, delivery.Body); err == nil {
				_ = delivery.Ack(false)
				continue
			} else if retryCount(delivery.Headers) >= 3 {
				deadLetter := amqp091.Publishing{
					Headers: delivery.Headers, ContentType: delivery.ContentType, ContentEncoding: delivery.ContentEncoding,
					DeliveryMode: delivery.DeliveryMode, Priority: delivery.Priority, CorrelationId: delivery.CorrelationId,
					ReplyTo: delivery.ReplyTo, Expiration: delivery.Expiration, MessageId: delivery.MessageId,
					Timestamp: delivery.Timestamp, Type: delivery.Type, UserId: delivery.UserId, AppId: delivery.AppId,
					Body: delivery.Body,
				}
				if publishErr := channel.PublishWithContext(ctx, deadExchange, deadQueue, true, false, deadLetter); publishErr != nil {
					_ = delivery.Nack(false, false)
					continue
				}
				select {
				case <-ctx.Done():
					_ = delivery.Nack(false, false)
					return ctx.Err()
				case confirmation, ok := <-confirmations:
					if !ok || !confirmation.Ack {
						_ = delivery.Nack(false, false)
						continue
					}
				}
				select {
				case returned, ok := <-returns:
					if ok && returned.MessageId == deadLetter.MessageId {
						_ = delivery.Nack(false, false)
						continue
					}
				default:
				}
				slog.Error("account.created moved to dead-letter queue", "message_id", delivery.MessageId, "error", err)
				_ = delivery.Ack(false)
				continue
			}
			_ = delivery.Nack(false, false)
		}
	}
}

func handleAccountCreated(ctx context.Context, db *sql.DB, body []byte) error {
	var event accountCreated
	if err := json.Unmarshal(body, &event); err != nil || event.EventID == "" || event.AccountID < 1 || event.DisplayName == "" {
		return errors.New("invalid account.created event")
	}
	_, err := db.ExecContext(ctx, `INSERT INTO users (account_id, display_name, created_at, updated_at) VALUES (?, ?, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)) ON DUPLICATE KEY UPDATE account_id = VALUES(account_id)`, event.AccountID, event.DisplayName)
	if err != nil {
		return fmt.Errorf("create user profile: %w", err)
	}
	return nil
}

func retryCount(headers amqp091.Table) int64 {
	entries, ok := headers["x-death"].([]any)
	if !ok {
		return 0
	}
	for _, value := range entries {
		entry, ok := value.(amqp091.Table)
		if !ok || entry["reason"] != "rejected" {
			continue
		}
		if count, ok := entry["count"].(int64); ok {
			return count
		}
	}
	return 0
}
