package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minhphu05/finsights-backend/gen/go/finsights/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Service struct {
	finsightsv1.UnimplementedMarketServiceServer
	db *sql.DB
}

func New(db *sql.DB) *Service { return &Service{db: db} }

const latestQuoteJoin = `LEFT JOIN stock_quotes q ON q.id = (
	SELECT q2.id FROM stock_quotes q2 WHERE q2.symbol = i.symbol ORDER BY q2.quoted_at DESC LIMIT 1
)`

const stockSelect = `SELECT i.symbol, i.name, COALESCE(i.short_name, ''), COALESCE(i.sector, ''), i.exchange,
	COALESCE(CAST(q.price AS CHAR), '0'), COALESCE(CAST(q.change_percent AS CHAR), '0'),
	COALESCE(CAST(q.volume AS CHAR), '0'), COALESCE(CAST(i.market_cap AS CHAR), '0'),
	COALESCE(UNIX_TIMESTAMP(q.quoted_at) * 1000, 0)
	FROM instruments i ` + latestQuoteJoin

func (s *Service) ListStocks(ctx context.Context, request *finsightsv1.ListStocksRequest) (*finsightsv1.ListStocksResponse, error) {
	where, args := stockFilters(request)
	var total uint64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM instruments i `+where, args...).Scan(&total); err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	limit, offset := pageBounds(request.GetLimit(), request.GetOffset(), 100, 30)
	rows, err := s.db.QueryContext(ctx, stockSelect+where+` ORDER BY i.symbol LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.ListStocksResponse{Total: total}
	for rows.Next() {
		stock, err := scanStock(rows)
		if err != nil {
			return nil, status.Error(codes.Internal, "could not read stock data")
		}
		response.Stocks = append(response.Stocks, stock)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	return response, nil
}

func (s *Service) GetStock(ctx context.Context, request *finsightsv1.GetStockRequest) (*finsightsv1.Stock, error) {
	symbol := strings.ToUpper(strings.TrimSpace(request.GetSymbol()))
	stock, err := scanStock(s.db.QueryRowContext(ctx, stockSelect+` WHERE i.symbol = ? LIMIT 1`, symbol))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "stock not found")
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	return stock, nil
}

func (s *Service) GetStockCandles(ctx context.Context, request *finsightsv1.GetStockCandlesRequest) (*finsightsv1.GetStockCandlesResponse, error) {
	symbol := strings.ToUpper(strings.TrimSpace(request.GetSymbol()))
	if symbol == "" {
		return nil, status.Error(codes.InvalidArgument, "symbol is required")
	}
	limit, _ := pageBounds(request.GetLimit(), 0, 1000, 90)
	start, err := rangeStart(request.GetRange())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "unsupported candle range")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT UNIX_TIMESTAMP(c.candle_at) * 1000, CAST(c.open_price AS CHAR), CAST(c.high_price AS CHAR), CAST(c.low_price AS CHAR), CAST(c.close_price AS CHAR), CAST(c.volume AS CHAR)
		FROM stock_candles c WHERE c.symbol = ? AND c.candle_at >= ? ORDER BY c.candle_at DESC LIMIT ?`, symbol, start, limit)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.GetStockCandlesResponse{}
	for rows.Next() {
		candle := &finsightsv1.Candle{}
		if err := rows.Scan(&candle.TimeUnixMs, &candle.Open, &candle.High, &candle.Low, &candle.Close, &candle.Volume); err != nil {
			return nil, status.Error(codes.Internal, "could not read candle data")
		}
		response.Candles = append(response.Candles, candle)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	for left, right := 0, len(response.Candles)-1; left < right; left, right = left+1, right-1 {
		response.Candles[left], response.Candles[right] = response.Candles[right], response.Candles[left]
	}
	return response, nil
}

func (s *Service) ListIndices(ctx context.Context, _ *finsightsv1.ListIndicesRequest) (*finsightsv1.ListIndicesResponse, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT symbol, name, CAST(value AS CHAR), CAST(change_value AS CHAR), CAST(change_percent AS CHAR), exchange, CAST(volume AS CHAR), UNIX_TIMESTAMP(quoted_at) * 1000
		FROM market_indices ORDER BY symbol`)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.ListIndicesResponse{}
	for rows.Next() {
		index := &finsightsv1.MarketIndex{}
		if err := rows.Scan(&index.Symbol, &index.Name, &index.Value, &index.Change, &index.ChangePercent, &index.Exchange, &index.Volume, &index.QuoteAtUnixMs); err != nil {
			return nil, status.Error(codes.Internal, "could not read index data")
		}
		response.Indices = append(response.Indices, index)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	return response, nil
}

func (s *Service) ListNews(ctx context.Context, request *finsightsv1.ListNewsRequest) (*finsightsv1.ListNewsResponse, error) {
	where, args := newsFilters(request)
	var total uint64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT a.id) FROM news_articles a LEFT JOIN news_article_symbols ns ON ns.article_id = a.id `+where, args...).Scan(&total); err != nil {
		return nil, status.Error(codes.Unavailable, "news storage is unavailable")
	}
	limit, offset := pageBounds(request.GetLimit(), request.GetOffset(), 100, 20)
	query := articleSelect + ` LEFT JOIN news_article_symbols ns ON ns.article_id = a.id ` + where + ` GROUP BY a.id ORDER BY a.published_at DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "news storage is unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.ListNewsResponse{Total: total}
	for rows.Next() {
		article, err := scanArticle(rows)
		if err != nil {
			return nil, status.Error(codes.Internal, "could not read news")
		}
		response.Articles = append(response.Articles, article)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Error(codes.Unavailable, "news storage is unavailable")
	}
	return response, nil
}

func (s *Service) GetNews(ctx context.Context, request *finsightsv1.GetNewsRequest) (*finsightsv1.NewsArticle, error) {
	article, err := scanArticle(s.db.QueryRowContext(ctx, articleSelect+` LEFT JOIN news_article_symbols ns ON ns.article_id = a.id WHERE a.slug = ? GROUP BY a.id LIMIT 1`, request.GetSlug()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "article not found")
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "news storage is unavailable")
	}
	return article, nil
}

const articleSelect = `SELECT a.slug, a.title, a.description, a.category,
	COALESCE(GROUP_CONCAT(DISTINCT ns.symbol ORDER BY ns.symbol SEPARATOR ','), ''),
	COALESCE(a.image_url, ''), COALESCE(a.source_name, ''), COALESCE(a.source_url, ''),
	UNIX_TIMESTAMP(a.published_at) * 1000, COALESCE(a.body_markdown, '') FROM news_articles a`

type rowScanner interface{ Scan(...any) error }

func scanStock(scanner rowScanner) (*finsightsv1.Stock, error) {
	stock := &finsightsv1.Stock{}
	err := scanner.Scan(&stock.Symbol, &stock.Name, &stock.ShortName, &stock.Sector, &stock.Exchange, &stock.Price, &stock.Change, &stock.Volume, &stock.MarketCap, &stock.QuoteAtUnixMs)
	return stock, err
}

func scanArticle(scanner rowScanner) (*finsightsv1.NewsArticle, error) {
	article := &finsightsv1.NewsArticle{}
	var symbols string
	err := scanner.Scan(&article.Slug, &article.Title, &article.Description, &article.Category, &symbols, &article.ImageUrl, &article.SourceName, &article.SourceUrl, &article.PublishedAtUnixMs, &article.BodyMarkdown)
	if err != nil {
		return nil, err
	}
	if symbols != "" {
		article.Symbols = strings.Split(symbols, ",")
	}
	return article, nil
}

func stockFilters(request *finsightsv1.ListStocksRequest) (string, []any) {
	clauses := []string{"1 = 1"}
	args := []any{}
	if query := strings.TrimSpace(request.GetQuery()); query != "" {
		clauses = append(clauses, `(i.symbol LIKE ? OR i.name LIKE ? OR i.short_name LIKE ?)`)
		like := "%" + query + "%"
		args = append(args, like, like, like)
	}
	if exchange := strings.ToUpper(strings.TrimSpace(request.GetExchange())); exchange != "" {
		clauses = append(clauses, `i.exchange = ?`)
		args = append(args, exchange)
	}
	if sector := strings.TrimSpace(request.GetSector()); sector != "" {
		clauses = append(clauses, `i.sector = ?`)
		args = append(args, sector)
	}
	return `WHERE ` + strings.Join(clauses, ` AND `), args
}

func newsFilters(request *finsightsv1.ListNewsRequest) (string, []any) {
	clauses := []string{"a.published_at <= UTC_TIMESTAMP(3)"}
	args := []any{}
	if symbol := strings.ToUpper(strings.TrimSpace(request.GetSymbol())); symbol != "" {
		clauses = append(clauses, `ns.symbol = ?`)
		args = append(args, symbol)
	}
	if category := strings.TrimSpace(request.GetCategory()); category != "" {
		clauses = append(clauses, `a.category = ?`)
		args = append(args, category)
	}
	if query := strings.TrimSpace(request.GetQuery()); query != "" {
		clauses = append(clauses, `(a.title LIKE ? OR a.description LIKE ?)`)
		like := "%" + query + "%"
		args = append(args, like, like)
	}
	return `WHERE ` + strings.Join(clauses, ` AND `), args
}

func pageBounds(limit, offset, max, fallback uint32) (uint32, uint32) {
	if limit == 0 {
		limit = fallback
	}
	if limit > max {
		limit = max
	}
	return limit, offset
}

func rangeStart(value string) (time.Time, error) {
	now := time.Now().UTC()
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "1D":
		return now.AddDate(0, 0, -1), nil
	case "1W":
		return now.AddDate(0, 0, -7), nil
	case "1M":
		return now.AddDate(0, -1, 0), nil
	case "3M":
		return now.AddDate(0, -3, 0), nil
	case "1Y", "":
		return now.AddDate(-1, 0, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unknown range %q", value)
	}
}
