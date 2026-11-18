package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	finsightsv1 "github.com/minhphu05/finsights-backend/gen/go/finsights/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Service struct {
	finsightsv1.UnimplementedMarketServiceServer
	marketDB       *sql.DB
	newsDB         *sql.DB
	fundamentalsDB *sql.DB
	disclosuresDB  *sql.DB
}

func New(marketDB, newsDB, fundamentalsDB, disclosuresDB *sql.DB) *Service {
	return &Service{marketDB: marketDB, newsDB: newsDB, fundamentalsDB: fundamentalsDB, disclosuresDB: disclosuresDB}
}

const stockFrom = `FROM market_securities s
	JOIN market_exchanges e ON e.id=s.exchange_id
	LEFT JOIN market_companies c ON c.id=s.company_id
	LEFT JOIN market_industries industry ON industry.id=c.industry_id
	LEFT JOIN market_quotes_latest q ON q.security_id=s.id
	LEFT JOIN market_prices_daily p ON p.id=(
		SELECT p2.id FROM market_prices_daily p2
		JOIN market_providers provider2 ON provider2.id=p2.provider_id
		WHERE p2.security_id=s.id AND provider2.code='cophieu68'
		ORDER BY p2.trading_date DESC LIMIT 1
	)
	LEFT JOIN market_prices_daily previous_price ON previous_price.id=(
		SELECT p3.id FROM market_prices_daily p3
		WHERE p3.security_id=s.id AND p3.provider_id=p.provider_id AND p3.trading_date<p.trading_date
		ORDER BY p3.trading_date DESC LIMIT 1
	)
	LEFT JOIN market_valuations_daily valuation ON valuation.id=(
		SELECT v2.id FROM market_valuations_daily v2 WHERE v2.security_id=s.id ORDER BY v2.trading_date DESC LIMIT 1
	)`

const stockSelect = `SELECT s.symbol,
	COALESCE(NULLIF(c.legal_name,''),NULLIF(s.name,''),s.symbol),
	COALESCE(NULLIF(c.short_name,''),NULLIF(s.name,''),s.symbol),
	COALESCE(industry.name_vi,''),COALESCE(e.code,''),
	COALESCE(CAST(q.last_price_vnd AS CHAR),CAST(p.close_vnd AS CHAR),'0'),
	COALESCE(CAST(q.change_percent AS CHAR),CAST(p.change_percent AS CHAR),
		CAST((p.close_vnd-previous_price.close_vnd)/NULLIF(previous_price.close_vnd,0)*100 AS CHAR),'0'),
	COALESCE(CAST(q.total_volume AS CHAR),CAST(p.matched_volume AS CHAR),'0'),
	COALESCE(CAST(valuation.market_cap_vnd AS CHAR),''),
	COALESCE(UNIX_TIMESTAMP(q.quote_at)*1000,UNIX_TIMESTAMP(p.trading_date)*1000,0)
	` + stockFrom

func (s *Service) ListStocks(ctx context.Context, request *finsightsv1.ListStocksRequest) (*finsightsv1.ListStocksResponse, error) {
	where, args := stockFilters(request)
	where += ` AND s.trading_status <> 'delisted'`
	var total uint64
	if err := s.marketDB.QueryRowContext(ctx, `SELECT COUNT(*) `+stockFrom+where, args...).Scan(&total); err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	limit, offset := pageBounds(request.GetLimit(), request.GetOffset(), 100, 30)
	rows, err := s.marketDB.QueryContext(ctx, stockSelect+where+` ORDER BY s.symbol LIMIT ? OFFSET ?`, append(args, limit, offset)...)
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
	if symbol == "" {
		return nil, status.Error(codes.InvalidArgument, "symbol is required")
	}
	stock, err := scanStock(s.marketDB.QueryRowContext(ctx, stockSelect+` WHERE s.symbol=? AND s.trading_status <> 'delisted' LIMIT 1`, symbol))
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
	limit, err := candleLimit(request.GetRange(), request.GetLimit())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "unsupported candle range")
	}
	query := `SELECT UNIX_TIMESTAMP(p.trading_date)*1000,COALESCE(CAST(p.open_vnd AS CHAR),'0'),
		COALESCE(CAST(p.high_vnd AS CHAR),'0'),COALESCE(CAST(p.low_vnd AS CHAR),'0'),
		CAST(p.close_vnd AS CHAR),COALESCE(CAST(p.matched_volume AS CHAR),'0')
		FROM market_prices_daily p JOIN market_securities s ON s.id=p.security_id
		JOIN market_providers provider ON provider.id=p.provider_id
		WHERE s.symbol=? AND provider.code='cophieu68'
		ORDER BY p.trading_date DESC LIMIT ?`
	rows, err := s.marketDB.QueryContext(ctx, query, symbol, limit)
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

func (s *Service) GetIndexCandles(ctx context.Context, request *finsightsv1.GetIndexCandlesRequest) (*finsightsv1.GetStockCandlesResponse, error) {
	symbol := strings.ToUpper(strings.TrimSpace(request.GetSymbol()))
	if symbol == "" {
		return nil, status.Error(codes.InvalidArgument, "index symbol is required")
	}
	limit, err := candleLimit(request.GetRange(), request.GetLimit())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "unsupported candle range")
	}
	query := `SELECT UNIX_TIMESTAMP(p.trading_date)*1000,COALESCE(CAST(p.open_points AS CHAR),'0'),
		COALESCE(CAST(p.high_points AS CHAR),'0'),COALESCE(CAST(p.low_points AS CHAR),'0'),
		CAST(p.close_points AS CHAR),COALESCE(CAST(p.total_volume AS CHAR),'0')
		FROM market_index_prices_daily p JOIN market_indices i ON i.id=p.index_id
		WHERE i.code=? ORDER BY p.trading_date DESC LIMIT ?`
	rows, err := s.marketDB.QueryContext(ctx, query, symbol, limit)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "market storage is unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.GetStockCandlesResponse{}
	for rows.Next() {
		candle := &finsightsv1.Candle{}
		if err := rows.Scan(&candle.TimeUnixMs, &candle.Open, &candle.High, &candle.Low, &candle.Close, &candle.Volume); err != nil {
			return nil, status.Error(codes.Internal, "could not read index candle data")
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

func candleLimit(value string, requested uint32) (uint32, error) {
	var fallback uint32
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "1D":
		fallback = 1
	case "1W":
		fallback = 5
	case "1M":
		fallback = 22
	case "3M":
		fallback = 66
	case "1Y":
		fallback = 250
	case "ALL", "MAX":
		fallback = 1000
	case "":
		fallback = 90
	default:
		return 0, fmt.Errorf("unknown candle range %q", value)
	}
	if requested == 0 {
		return fallback, nil
	}
	if requested > 1000 {
		return 1000, nil
	}
	return requested, nil
}

func (s *Service) ListIndices(ctx context.Context, _ *finsightsv1.ListIndicesRequest) (*finsightsv1.ListIndicesResponse, error) {
	query := `SELECT idx.code,idx.name,
		COALESCE(CAST(snapshot.index_value AS CHAR),CAST(daily.close_points AS CHAR),'0'),
		COALESCE(CAST(snapshot.change_points AS CHAR),CAST(daily.change_points AS CHAR),'0'),
		COALESCE(CAST(snapshot.change_percent AS CHAR),CAST(daily.change_percent AS CHAR),'0'),
		COALESCE(exchange.code,''),COALESCE(CAST(snapshot.total_volume AS CHAR),CAST(daily.total_volume AS CHAR),'0'),
		COALESCE(UNIX_TIMESTAMP(snapshot.observed_at)*1000,UNIX_TIMESTAMP(daily.trading_date)*1000,0)
		FROM market_indices idx LEFT JOIN market_exchanges exchange ON exchange.id=idx.exchange_id
		LEFT JOIN market_index_snapshots snapshot ON snapshot.index_id=idx.id AND snapshot.observed_at=(
			SELECT MAX(s2.observed_at) FROM market_index_snapshots s2 WHERE s2.index_id=idx.id)
		LEFT JOIN market_index_prices_daily daily ON daily.id=(
			SELECT d2.id FROM market_index_prices_daily d2 WHERE d2.index_id=idx.id ORDER BY d2.trading_date DESC LIMIT 1)
		WHERE idx.is_active=1 AND (snapshot.index_value IS NOT NULL OR daily.close_points IS NOT NULL)
		ORDER BY idx.code`
	rows, err := s.marketDB.QueryContext(ctx, query)
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
	if err := s.newsDB.QueryRowContext(ctx, `SELECT COUNT(DISTINCT a.id) `+articleFrom+where, args...).Scan(&total); err != nil {
		slog.ErrorContext(ctx, "news count query failed", "error", err)
		return nil, status.Error(codes.Unavailable, "news storage is unavailable")
	}
	limit, offset := pageBounds(request.GetLimit(), request.GetOffset(), 100, 20)
	query := articleSelect + articleFrom + where + `
		GROUP BY a.id,pub.code ORDER BY a.published_at DESC,a.id DESC LIMIT ? OFFSET ?`
	rows, err := s.newsDB.QueryContext(ctx, query, append(args, limit, offset)...)
	if err != nil {
		slog.ErrorContext(ctx, "news list query failed", "error", err)
		return nil, status.Error(codes.Unavailable, "news storage is unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.ListNewsResponse{Total: total}
	for rows.Next() {
		article, err := scanArticle(rows)
		if err != nil {
			slog.ErrorContext(ctx, "news row scan failed", "error", err)
			return nil, status.Error(codes.Internal, "could not read news")
		}
		response.Articles = append(response.Articles, article)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "news rows failed", "error", err)
		return nil, status.Error(codes.Unavailable, "news storage is unavailable")
	}
	return response, nil
}

func (s *Service) GetNews(ctx context.Context, request *finsightsv1.GetNewsRequest) (*finsightsv1.NewsArticle, error) {
	slug := strings.TrimSpace(request.GetSlug())
	if slug == "" {
		return nil, status.Error(codes.InvalidArgument, "article slug is required")
	}
	query := articleSelect + articleFrom + ` WHERE a.article_status='published' AND
		(a.slug=? OR CONCAT(pub.code,'-',LOWER(SUBSTRING(SHA2(a.canonical_url,256),1,20)))=?)
		GROUP BY a.id,pub.code LIMIT 1`
	article, err := scanArticle(s.newsDB.QueryRowContext(ctx, query, slug, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "article not found")
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "news storage is unavailable")
	}
	return article, nil
}

const articleFrom = `FROM news_articles a
	JOIN news_publishers pub ON pub.id=a.publisher_id
	LEFT JOIN news_article_securities ns ON ns.article_id=a.id
	LEFT JOIN news_article_categories ac ON ac.article_id=a.id AND ac.is_primary=1
	LEFT JOIN news_categories cat ON cat.id=ac.category_id`
const articleSelect = `SELECT COALESCE(NULLIF(a.slug,''),CONCAT(pub.code,'-',LOWER(SUBSTRING(SHA2(a.canonical_url,256),1,20)))),
	a.title,COALESCE(a.summary,a.excerpt,''),COALESCE(MAX(cat.name),MAX(a.original_section),'Tin tức'),
	COALESCE(GROUP_CONCAT(DISTINCT ns.symbol_at_publication ORDER BY ns.symbol_at_publication SEPARATOR ','),''),
	COALESCE(a.cover_image_url,''),pub.name,COALESCE(a.canonical_url,''),
	COALESCE(UNIX_TIMESTAMP(a.published_at)*1000,0),'',pub.code`

func (s *Service) GetStockFinancials(ctx context.Context, request *finsightsv1.GetStockFinancialsRequest) (*finsightsv1.GetStockFinancialsResponse, error) {
	symbol := strings.ToUpper(strings.TrimSpace(request.GetSymbol()))
	if symbol == "" {
		return nil, status.Error(codes.InvalidArgument, "symbol is required")
	}
	periodType := strings.ToLower(strings.TrimSpace(request.GetPeriodType()))
	if periodType != "" && periodType != "quarter" && periodType != "year" {
		return nil, status.Error(codes.InvalidArgument, "period_type must be quarter or year")
	}
	limit := request.GetLimit()
	if limit == 0 {
		limit = 24
	}
	if limit > 100 {
		limit = 100
	}
	query := `SELECT p.period_code,p.fiscal_year,COALESCE(p.fiscal_quarter,0),p.period_type,
		p.period_end,p.currency,p.audit_status,v.line_key,COALESCE(v.source_label,''),
		COALESCE(m.canonical_name_vi,v.source_label,''),COALESCE(m.statement_family,''),
		COALESCE(m.metric_group,''),COALESCE(CAST(v.value_decimal AS CHAR),''),
		COALESCE(CAST(v.original_value_decimal AS CHAR),''),COALESCE(v.reported_unit,m.unit_code,''),
		COALESCE(CAST(v.unit_multiplier AS CHAR),'1'),COALESCE(v.source_currency,p.currency,''),
		COALESCE(v.row_order,0)
		FROM (
			SELECT id,period_code,fiscal_year,fiscal_quarter,period_type,period_end,currency,audit_status
			FROM financial_periods WHERE source_code='cophieu68' AND source_company_id=? AND is_current=1
			  AND (?='' OR period_type=?) ORDER BY period_end DESC LIMIT ?
		) p LEFT JOIN financial_values v ON v.period_id=p.id
		LEFT JOIN financial_metrics m ON m.id=v.metric_id
		ORDER BY p.period_end DESC,COALESCE(v.row_order,0),v.line_key`
	rows, err := s.fundamentalsDB.QueryContext(ctx, query, symbol, periodType, periodType, limit)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "financial data is unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.GetStockFinancialsResponse{Symbol: symbol}
	periodByCode := make(map[string]*finsightsv1.FinancialPeriod)
	for rows.Next() {
		var code, kind, currency, audit string
		var fiscalYear, fiscalQuarter uint32
		var end time.Time
		var lineKey, sourceLabel, canonicalName, family, group, value, original, unit, multiplier, valueCurrency sql.NullString
		var rowOrder uint32
		if err := rows.Scan(&code, &fiscalYear, &fiscalQuarter, &kind, &end, &currency, &audit,
			&lineKey, &sourceLabel, &canonicalName, &family, &group, &value, &original, &unit, &multiplier, &valueCurrency, &rowOrder); err != nil {
			return nil, status.Error(codes.Internal, "could not read financial data")
		}
		period := periodByCode[code]
		if period == nil {
			period = &finsightsv1.FinancialPeriod{PeriodCode: code, FiscalYear: fiscalYear, FiscalQuarter: fiscalQuarter,
				PeriodType: kind, PeriodEnd: end.Format("2006-01-02"), Currency: currency, AuditStatus: audit}
			periodByCode[code] = period
			response.Periods = append(response.Periods, period)
		}
		if lineKey.Valid {
			period.Values = append(period.Values, &finsightsv1.FinancialValue{LineKey: lineKey.String, SourceLabel: sourceLabel.String,
				CanonicalName: canonicalName.String, StatementFamily: family.String, MetricGroup: group.String, Value: value.String,
				OriginalValue: original.String, Unit: unit.String, UnitMultiplier: multiplier.String, Currency: valueCurrency.String, RowOrder: rowOrder})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, status.Error(codes.Unavailable, "financial data is unavailable")
	}
	return response, nil
}

func (s *Service) ListFinancialDocuments(ctx context.Context, request *finsightsv1.ListFinancialDocumentsRequest) (*finsightsv1.ListFinancialDocumentsResponse, error) {
	symbol := strings.ToUpper(strings.TrimSpace(request.GetSymbol()))
	kind := strings.TrimSpace(request.GetKind())
	limit, offset := pageBounds(request.GetLimit(), request.GetOffset(), 100, 30)
	var securityUID, companyUID string
	if symbol != "" {
		err := s.marketDB.QueryRowContext(ctx, `SELECT s.security_uid,COALESCE(c.company_uid,'') FROM market_securities s
			LEFT JOIN market_companies c ON c.id=s.company_id
			WHERE s.symbol=? AND s.trading_status <> 'delisted' ORDER BY s.id LIMIT 1`, symbol).Scan(&securityUID, &companyUID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "stock not found")
		}
		if err != nil {
			return nil, status.Error(codes.Unavailable, "market storage is unavailable")
		}
	}
	where := `WHERE d.record_status='published'`
	args := []any{}
	if kind != "" && !strings.EqualFold(kind, "all") {
		where += ` AND d.disclosure_kind=?`
		args = append(args, kind)
	}
	if securityUID != "" && companyUID != "" {
		where += ` AND (d.security_uid=? OR d.company_uid=?)`
		args = append(args, securityUID, companyUID)
	} else if securityUID != "" {
		where += ` AND d.security_uid=?`
		args = append(args, securityUID)
	}
	var total uint64
	if err := s.disclosuresDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM issuer_disclosures d `+where, args...).Scan(&total); err != nil {
		slog.ErrorContext(ctx, "financial document count query failed", "error", err)
		return nil, status.Error(codes.Unavailable, "financial documents are unavailable")
	}
	query := `SELECT d.disclosure_uid,COALESCE(d.company_uid,''),COALESCE(d.security_uid,''),d.source_code,d.disclosure_kind,d.title,COALESCE(d.summary,''),
		COALESCE(CAST(UNIX_TIMESTAMP(d.published_at)*1000 AS UNSIGNED),0),COALESCE(d.original_url,''),
		COALESCE(d.mime_type,''),COALESCE(d.document_asset_uid,''),d.rights_status
		FROM issuer_disclosures d ` + where + ` ORDER BY d.published_at DESC,d.id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.disclosuresDB.QueryContext(ctx, query, args...)
	if err != nil {
		slog.ErrorContext(ctx, "financial document list query failed", "error", err)
		return nil, status.Error(codes.Unavailable, "financial documents are unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.ListFinancialDocumentsResponse{Total: total}
	type documentRow struct {
		document    *finsightsv1.FinancialDocument
		companyUID  string
		securityUID string
	}
	documents := make([]documentRow, 0, limit)
	for rows.Next() {
		doc := &finsightsv1.FinancialDocument{Symbol: symbol}
		var companyUID, securityUID string
		if err := rows.Scan(&doc.Uid, &companyUID, &securityUID, &doc.SourceCode, &doc.Kind, &doc.Title, &doc.Summary, &doc.PublishedAtUnixMs,
			&doc.OriginalUrl, &doc.MimeType, &doc.DocumentAssetUid, &doc.RightsStatus); err != nil {
			slog.ErrorContext(ctx, "financial document row scan failed", "error", err)
			return nil, status.Error(codes.Internal, "could not read financial documents")
		}
		documents = append(documents, documentRow{document: doc, companyUID: companyUID, securityUID: securityUID})
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "financial document rows failed", "error", err)
		return nil, status.Error(codes.Unavailable, "financial documents are unavailable")
	}
	if symbol == "" {
		securityUIDs := make([]string, 0, len(documents))
		companyUIDs := make([]string, 0, len(documents))
		securitySeen := make(map[string]struct{})
		companySeen := make(map[string]struct{})
		for _, item := range documents {
			if item.securityUID != "" {
				if _, exists := securitySeen[item.securityUID]; !exists {
					securitySeen[item.securityUID] = struct{}{}
					securityUIDs = append(securityUIDs, item.securityUID)
				}
			}
			if item.companyUID != "" {
				if _, exists := companySeen[item.companyUID]; !exists {
					companySeen[item.companyUID] = struct{}{}
					companyUIDs = append(companyUIDs, item.companyUID)
				}
			}
		}
		if len(securityUIDs) > 0 {
			placeholders := strings.TrimRight(strings.Repeat("?,", len(securityUIDs)), ",")
			query := `SELECT security_uid,symbol FROM market_securities WHERE security_uid IN (` + placeholders + `)`
			uidArgs := make([]any, len(securityUIDs))
			for i, uid := range securityUIDs {
				uidArgs[i] = uid
			}
			mappingRows, mapErr := s.marketDB.QueryContext(ctx, query, uidArgs...)
			if mapErr != nil {
				return nil, status.Error(codes.Unavailable, "market storage is unavailable")
			}
			for mappingRows.Next() {
				var uid, symbol string
				if scanErr := mappingRows.Scan(&uid, &symbol); scanErr != nil {
					mappingRows.Close()
					return nil, status.Error(codes.Internal, "could not map report companies")
				}
				for _, item := range documents {
					if item.securityUID == uid {
						item.document.Symbol = symbol
					}
				}
			}
			if mapErr := mappingRows.Err(); mapErr != nil {
				mappingRows.Close()
				return nil, status.Error(codes.Unavailable, "market storage is unavailable")
			}
			mappingRows.Close()
		}
		if len(companyUIDs) > 0 {
			placeholders := strings.TrimRight(strings.Repeat("?,", len(companyUIDs)), ",")
			query := `SELECT c.company_uid,MIN(s.symbol) FROM market_companies c
				JOIN market_securities s ON s.company_id=c.id
				WHERE c.company_uid IN (` + placeholders + `) AND s.trading_status <> 'delisted' GROUP BY c.company_uid`
			uidArgs := make([]any, len(companyUIDs))
			for i, uid := range companyUIDs {
				uidArgs[i] = uid
			}
			mappingRows, mapErr := s.marketDB.QueryContext(ctx, query, uidArgs...)
			if mapErr != nil {
				return nil, status.Error(codes.Unavailable, "market storage is unavailable")
			}
			for mappingRows.Next() {
				var uid, symbol string
				if scanErr := mappingRows.Scan(&uid, &symbol); scanErr != nil {
					mappingRows.Close()
					return nil, status.Error(codes.Internal, "could not map report companies")
				}
				for _, item := range documents {
					if item.companyUID == uid && item.document.Symbol == "" {
						item.document.Symbol = symbol
					}
				}
			}
			if mapErr := mappingRows.Err(); mapErr != nil {
				mappingRows.Close()
				return nil, status.Error(codes.Unavailable, "market storage is unavailable")
			}
			mappingRows.Close()
		}
	}
	for _, item := range documents {
		response.Documents = append(response.Documents, item.document)
	}
	return response, nil
}

type rowScanner interface{ Scan(...any) error }

func scanStock(scanner rowScanner) (*finsightsv1.Stock, error) {
	stock := &finsightsv1.Stock{}
	err := scanner.Scan(&stock.Symbol, &stock.Name, &stock.ShortName, &stock.Sector, &stock.Exchange, &stock.Price,
		&stock.Change, &stock.Volume, &stock.MarketCap, &stock.QuoteAtUnixMs)
	return stock, err
}

func scanArticle(scanner rowScanner) (*finsightsv1.NewsArticle, error) {
	article := &finsightsv1.NewsArticle{}
	var symbols string
	err := scanner.Scan(&article.Slug, &article.Title, &article.Description, &article.Category, &symbols,
		&article.ImageUrl, &article.SourceName, &article.SourceUrl, &article.PublishedAtUnixMs, &article.BodyMarkdown, &article.PublisherCode)
	if err != nil {
		return nil, err
	}
	for _, symbol := range strings.Split(symbols, ",") {
		symbol = strings.TrimSpace(symbol)
		if symbol != "" {
			article.Symbols = append(article.Symbols, symbol)
		}
	}
	return article, nil
}

func stockFilters(request *finsightsv1.ListStocksRequest) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	if query := strings.TrimSpace(request.GetQuery()); query != "" {
		clauses = append(clauses, `(s.symbol LIKE ? OR s.name LIKE ? OR c.short_name LIKE ? OR c.legal_name LIKE ?)`)
		like := "%" + query + "%"
		args = append(args, like, like, like, like)
	}
	if exchange := strings.ToUpper(strings.TrimSpace(request.GetExchange())); exchange != "" {
		clauses = append(clauses, `e.code=?`)
		args = append(args, exchange)
	}
	if sector := strings.TrimSpace(request.GetSector()); sector != "" {
		clauses = append(clauses, `industry.name_vi=?`)
		args = append(args, sector)
	}
	return `WHERE ` + strings.Join(clauses, ` AND `), args
}

func newsFilters(request *finsightsv1.ListNewsRequest) (string, []any) {
	clauses := []string{"a.article_status='published'", "(a.published_at IS NULL OR a.published_at<=UTC_TIMESTAMP(3))"}
	args := []any{}
	if symbol := strings.ToUpper(strings.TrimSpace(request.GetSymbol())); symbol != "" {
		clauses = append(clauses, `ns.symbol_at_publication=?`)
		args = append(args, symbol)
	}
	if category := strings.TrimSpace(request.GetCategory()); category != "" {
		clauses = append(clauses, `COALESCE(cat.name,a.original_section)=?`)
		args = append(args, category)
	}
	if query := strings.TrimSpace(request.GetQuery()); query != "" {
		clauses = append(clauses, `(a.title LIKE ? OR a.summary LIKE ? OR a.excerpt LIKE ?)`)
		like := "%" + query + "%"
		args = append(args, like, like, like)
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
