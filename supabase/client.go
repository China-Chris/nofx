package supabase

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"nofx/config"
)

// Client wraps minimal Supabase REST interactions used by the API server.
type Client struct {
	baseURL       string
	serviceKey    string
	encryptionKey []byte
	httpClient    *http.Client
}

// NewClientFromEnv builds a Supabase client using environment variables.
func NewClientFromEnv() (*Client, error) {
	url := strings.TrimSuffix(os.Getenv("SUPABASE_URL"), "/")
	serviceKey := os.Getenv("SUPABASE_SERVICE_KEY")
	encryptionKey := os.Getenv("ENCRYPTION_KEY")

	if url == "" || serviceKey == "" || encryptionKey == "" {
		return nil, errors.New("missing SUPABASE_URL, SUPABASE_SERVICE_KEY or ENCRYPTION_KEY")
	}

	// Derive a fixed-length key for AES-256 using SHA-256.
	keyHash := sha256.Sum256([]byte(encryptionKey))

	return &Client{
		baseURL:    url,
		serviceKey: serviceKey,
		// Retain a copy to avoid accidental modification from outside.
		encryptionKey: append([]byte(nil), keyHash[:]...),
		httpClient:    &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// UpsertUserAPIKey encrypts the raw key and upserts it into Supabase.
func (c *Client) UpsertUserAPIKey(ctx context.Context, tgID, provider, rawKey string) error {
	if c == nil {
		return errors.New("supabase client is nil")
	}
	if strings.TrimSpace(tgID) == "" || strings.TrimSpace(provider) == "" || strings.TrimSpace(rawKey) == "" {
		return errors.New("tg_id, provider and raw key must be provided")
	}

	encrypted, err := c.encrypt(rawKey)
	if err != nil {
		return fmt.Errorf("encrypt key: %w", err)
	}

	payload := []map[string]string{{
		"tg_id":         strings.TrimSpace(tgID),
		"provider":      strings.TrimSpace(provider),
		"encrypted_key": encrypted,
	}}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal supabase payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/rest/v1/user_api_keys?on_conflict=tg_id,provider", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build supabase request: %w", err)
	}

	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "resolution=merge-duplicates,return=representation")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call supabase: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase upsert failed: status=%d body=%s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// UpsertUserAIModel 同步用户的 AI 模型配置
func (c *Client) UpsertUserAIModel(ctx context.Context, tgID string, model *config.AIModelConfig) error {
	if c == nil || model == nil {
		return nil
	}

	record := userAIModelRow{
		TGID:            strings.TrimSpace(tgID),
		ModelID:         strings.TrimSpace(model.ID),
		Provider:        strings.TrimSpace(model.Provider),
		Name:            strings.TrimSpace(model.Name),
		Enabled:         model.Enabled,
		CustomAPIURL:    strings.TrimSpace(model.CustomAPIURL),
		CustomModelName: strings.TrimSpace(model.CustomModelName),
		APIKeyHint:      keyHint(model.APIKey),
	}

	if record.TGID == "" || record.ModelID == "" {
		return nil
	}

	return c.upsertRecords(ctx, "user_ai_models", "tg_id,model_id", []userAIModelRow{record})
}

// UpsertUserExchange 同步用户的交易所配置
func (c *Client) UpsertUserExchange(ctx context.Context, tgID string, exchange *config.ExchangeConfig) error {
	if c == nil || exchange == nil {
		return nil
	}

	extra := map[string]interface{}{}
	if hk := keyHint(exchange.APIKey); hk != "" {
		extra["api_key_hint"] = hk
	}
	if sk := keyHint(exchange.SecretKey); sk != "" {
		extra["secret_key_hint"] = sk
	}
	if ak := keyHint(exchange.AsterPrivateKey); ak != "" {
		extra["aster_private_key_hint"] = ak
	}
	if len(extra) == 0 {
		extra = nil
	}

	record := userExchangeRow{
		TGID:                  strings.TrimSpace(tgID),
		ExchangeID:            strings.TrimSpace(exchange.ID),
		Name:                  strings.TrimSpace(exchange.Name),
		Type:                  strings.TrimSpace(exchange.Type),
		Enabled:               exchange.Enabled,
		Testnet:               exchange.Testnet,
		HyperliquidWalletAddr: strings.TrimSpace(exchange.HyperliquidWalletAddr),
		AsterUser:             strings.TrimSpace(exchange.AsterUser),
		AsterSigner:           strings.TrimSpace(exchange.AsterSigner),
		Extras:                extra,
	}

	if exchange.APIKey != "" {
		enc, err := c.encrypt(exchange.APIKey)
		if err != nil {
			return fmt.Errorf("encrypt exchange api key: %w", err)
		}
		record.APIKeyEncrypted = enc
	}

	if exchange.SecretKey != "" {
		enc, err := c.encrypt(exchange.SecretKey)
		if err != nil {
			return fmt.Errorf("encrypt exchange secret key: %w", err)
		}
		record.SecretKeyEncrypted = enc
	}

	if exchange.AsterPrivateKey != "" {
		enc, err := c.encrypt(exchange.AsterPrivateKey)
		if err != nil {
			return fmt.Errorf("encrypt aster private key: %w", err)
		}
		record.AsterPrivateEncrypted = enc
	}

	if record.TGID == "" || record.ExchangeID == "" {
		return nil
	}

	if err := c.upsertRecords(ctx, "user_exchanges", "tg_id,exchange_id", []userExchangeRow{record}); err != nil {
		return err
	}

	if strings.EqualFold(record.ExchangeID, "hyperliquid") && exchange.APIKey != "" {
		if err := c.UpsertHyperliquidKey(ctx, record.TGID, exchange.APIKey, exchange.HyperliquidWalletAddr); err != nil {
			return err
		}
	}

	return nil
}

// UpsertHyperliquidKey 将 Hyperliquid 私钥写入 user_keys
func (c *Client) UpsertHyperliquidKey(ctx context.Context, tgID, privateKey, walletAddr string) error {
	if c == nil {
		return nil
	}

	tgID = strings.TrimSpace(tgID)
	privateKey = strings.TrimSpace(privateKey)
	if tgID == "" || privateKey == "" {
		return nil
	}

	encKey, err := c.encrypt(privateKey)
	if err != nil {
		return fmt.Errorf("encrypt hyperliquid private key: %w", err)
	}

	payload := []map[string]string{{
		"tg_id":                 tgID,
		"key_type":              "hyperliquid",
		"encrypted_private_key": encKey,
		"wallet_address":        strings.TrimSpace(walletAddr),
	}}

	return c.upsertRecords(ctx, "user_keys", "tg_id,key_type", payload)
}

// UpsertUserTrader 同步单个交易员配置
func (c *Client) UpsertUserTrader(ctx context.Context, tgID string, trader *config.TraderRecord) error {
	if c == nil || trader == nil {
		return nil
	}

	extra := map[string]interface{}{
		"scan_interval_minutes": trader.ScanIntervalMinutes,
	}
	if trader.UseInsideCoins {
		extra["use_inside_coins"] = true
	}
	if trader.TradingSymbols != "" {
		extra["trading_symbols_raw"] = trader.TradingSymbols
	}

	record := userTraderRow{
		TGID:                 strings.TrimSpace(tgID),
		TraderCode:           strings.TrimSpace(trader.ID),
		DisplayName:          strings.TrimSpace(trader.Name),
		AIModelID:            strings.TrimSpace(trader.AIModelID),
		ExchangeID:           strings.TrimSpace(trader.ExchangeID),
		InitialBalance:       trader.InitialBalance,
		BTCETHLeverage:       trader.BTCETHLeverage,
		AltcoinLeverage:      trader.AltcoinLeverage,
		TradingSymbols:       strings.TrimSpace(trader.TradingSymbols),
		CustomPrompt:         strings.TrimSpace(trader.CustomPrompt),
		OverrideBasePrompt:   trader.OverrideBasePrompt,
		SystemPromptTemplate: strings.TrimSpace(trader.SystemPromptTemplate),
		IsCrossMargin:        trader.IsCrossMargin,
		UseCoinPool:          trader.UseCoinPool,
		UseOITop:             trader.UseOITop,
		Status:               traderStatus(trader.IsRunning),
		Extras:               extra,
	}

	if record.TGID == "" || record.TraderCode == "" {
		return nil
	}

	return c.upsertRecords(ctx, "user_traders", "tg_id,trader_code", []userTraderRow{record})
}

// DeleteUserTrader 从 Supabase 删除交易员
func (c *Client) DeleteUserTrader(ctx context.Context, tgID, traderCode string) error {
	if c == nil {
		return nil
	}
	filters := map[string]string{
		"tg_id":       strings.TrimSpace(tgID),
		"trader_code": strings.TrimSpace(traderCode),
	}
	return c.deleteRows(ctx, "user_traders", filters)
}

// UpsertUserSignalSource 同步用户信号源配置
func (c *Client) UpsertUserSignalSource(ctx context.Context, tgID string, source *config.UserSignalSource) error {
	if c == nil || source == nil {
		return nil
	}
	record := userSignalRow{
		TGID:        strings.TrimSpace(tgID),
		CoinPoolURL: strings.TrimSpace(source.CoinPoolURL),
		OITopURL:    strings.TrimSpace(source.OITopURL),
	}
	if record.TGID == "" {
		return nil
	}
	return c.upsertRecords(ctx, "user_signal_sources", "tg_id", []userSignalRow{record})
}

type userAIModelRow struct {
	TGID            string `json:"tg_id"`
	ModelID         string `json:"model_id"`
	Provider        string `json:"provider"`
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	CustomAPIURL    string `json:"custom_api_url,omitempty"`
	CustomModelName string `json:"custom_model_name,omitempty"`
	APIKeyHint      string `json:"api_key_hint,omitempty"`
}

type userExchangeRow struct {
	TGID                  string                 `json:"tg_id"`
	ExchangeID            string                 `json:"exchange_id"`
	Name                  string                 `json:"name"`
	Type                  string                 `json:"type"`
	Enabled               bool                   `json:"enabled"`
	Testnet               bool                   `json:"testnet"`
	HyperliquidWalletAddr string                 `json:"hyperliquid_wallet_addr,omitempty"`
	AsterUser             string                 `json:"aster_user,omitempty"`
	AsterSigner           string                 `json:"aster_signer,omitempty"`
	APIKeyEncrypted       string                 `json:"api_key_encrypted,omitempty"`
	SecretKeyEncrypted    string                 `json:"secret_key_encrypted,omitempty"`
	AsterPrivateEncrypted string                 `json:"aster_private_key_encrypted,omitempty"`
	Extras                map[string]interface{} `json:"extras,omitempty"`
}

type userTraderRow struct {
	TGID                 string                 `json:"tg_id"`
	TraderCode           string                 `json:"trader_code"`
	DisplayName          string                 `json:"display_name"`
	AIModelID            string                 `json:"ai_model_id"`
	ExchangeID           string                 `json:"exchange_id"`
	InitialBalance       float64                `json:"initial_balance"`
	BTCETHLeverage       int                    `json:"btc_eth_leverage"`
	AltcoinLeverage      int                    `json:"altcoin_leverage"`
	TradingSymbols       string                 `json:"trading_symbols,omitempty"`
	CustomPrompt         string                 `json:"custom_prompt,omitempty"`
	OverrideBasePrompt   bool                   `json:"override_base_prompt"`
	SystemPromptTemplate string                 `json:"system_prompt_template,omitempty"`
	IsCrossMargin        bool                   `json:"is_cross_margin"`
	UseCoinPool          bool                   `json:"use_coin_pool"`
	UseOITop             bool                   `json:"use_oi_top"`
	Status               string                 `json:"status"`
	Extras               map[string]interface{} `json:"extras,omitempty"`
}

type userSignalRow struct {
	TGID        string `json:"tg_id"`
	CoinPoolURL string `json:"coin_pool_url,omitempty"`
	OITopURL    string `json:"oi_top_url,omitempty"`
}

// UserDecisionRow 决策记录表结构
type UserDecisionRow struct {
	TGID            string          `json:"tg_id"`
	TraderCode      string          `json:"trader_code"`
	DecisionCycle   int             `json:"decision_cycle"`
	DecisionTime    time.Time       `json:"decision_time"`
	DecisionSummary string          `json:"decision_summary"`
	ExecutionLog    string          `json:"execution_log"`
	Success         bool            `json:"success"`
	ErrorMessage    string          `json:"error_message,omitempty"`
	RawPayload      json.RawMessage `json:"raw_payload"`
}

// UserTradeExecutionRow 单笔交易执行记录
type UserTradeExecutionRow struct {
	TGID          string          `json:"tg_id"`
	TraderCode    string          `json:"trader_code"`
	DecisionCycle int             `json:"decision_cycle"`
	Action        string          `json:"action"`
	Symbol        string          `json:"symbol"`
	Side          string          `json:"side"`
	Quantity      float64         `json:"quantity"`
	Price         float64         `json:"price"`
	Leverage      int             `json:"leverage"`
	Success       bool            `json:"success"`
	ErrorMessage  string          `json:"error_message,omitempty"`
	RawPayload    json.RawMessage `json:"raw_payload"`
	ExecutedAt    time.Time       `json:"executed_at"`
}

func (c *Client) GetUserTrader(ctx context.Context, tgID, traderCode string) (*config.TraderRecord, error) {
	if c == nil {
		return nil, errors.New("supabase client is nil")
	}

	params := url.Values{}
	params.Set("tg_id", "eq."+tgID)
	params.Add("trader_code", "eq."+traderCode)
	params.Set("select", "trader_code,display_name,ai_model_id,exchange_id,initial_balance,btc_eth_leverage,altcoin_leverage,trading_symbols,custom_prompt,override_base_prompt,system_prompt_template,is_cross_margin,use_coin_pool,use_oi_top,status,extras,created_at,updated_at")

	var rows []struct {
		TraderCode           string          `json:"trader_code"`
		DisplayName          string          `json:"display_name"`
		AIModelID            string          `json:"ai_model_id"`
		ExchangeID           string          `json:"exchange_id"`
		InitialBalance       float64         `json:"initial_balance"`
		BTCETHLeverage       int             `json:"btc_eth_leverage"`
		AltcoinLeverage      int             `json:"altcoin_leverage"`
		TradingSymbols       string          `json:"trading_symbols"`
		CustomPrompt         string          `json:"custom_prompt"`
		OverrideBasePrompt   bool            `json:"override_base_prompt"`
		SystemPromptTemplate string          `json:"system_prompt_template"`
		IsCrossMargin        bool            `json:"is_cross_margin"`
		UseCoinPool          bool            `json:"use_coin_pool"`
		UseOITop             bool            `json:"use_oi_top"`
		Status               string          `json:"status"`
		Extras               json.RawMessage `json:"extras"`
		CreatedAt            *time.Time      `json:"created_at"`
		UpdatedAt            *time.Time      `json:"updated_at"`
	}

	if err := c.fetchRecords(ctx, "user_traders", params, &rows); err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return nil, nil
	}

	row := rows[0]

	scanInterval := 3
	useInsideCoins := false

	if len(row.Extras) > 0 {
		var extras map[string]interface{}
		if err := json.Unmarshal(row.Extras, &extras); err == nil {
			if v, ok := extras["scan_interval_minutes"].(float64); ok {
				scanInterval = int(v)
			}
			if v, ok := extras["use_inside_coins"].(bool); ok {
				useInsideCoins = v
			}
			if v, ok := extras["trading_symbols_raw"].(string); ok && strings.TrimSpace(v) != "" {
				row.TradingSymbols = v
			}
		}
	}

	trader := &config.TraderRecord{
		ID:                   row.TraderCode,
		UserID:               tgID,
		Name:                 row.DisplayName,
		AIModelID:            row.AIModelID,
		ExchangeID:           row.ExchangeID,
		InitialBalance:       row.InitialBalance,
		ScanIntervalMinutes:  scanInterval,
		IsRunning:            strings.EqualFold(row.Status, "running"),
		BTCETHLeverage:       row.BTCETHLeverage,
		AltcoinLeverage:      row.AltcoinLeverage,
		TradingSymbols:       row.TradingSymbols,
		UseCoinPool:          row.UseCoinPool,
		UseOITop:             row.UseOITop,
		UseInsideCoins:       useInsideCoins,
		CustomPrompt:         row.CustomPrompt,
		OverrideBasePrompt:   row.OverrideBasePrompt,
		SystemPromptTemplate: row.SystemPromptTemplate,
		IsCrossMargin:        row.IsCrossMargin,
	}

	if row.CreatedAt != nil {
		trader.CreatedAt = *row.CreatedAt
	}
	if row.UpdatedAt != nil {
		trader.UpdatedAt = *row.UpdatedAt
	}

	return trader, nil
}

// InsertUserTraderDecision 写入用户交易员决策记录
func (c *Client) InsertUserTraderDecision(ctx context.Context, row *UserDecisionRow) error {
	if c == nil || row == nil {
		return nil
	}

	row.TGID = strings.TrimSpace(row.TGID)
	row.TraderCode = strings.TrimSpace(row.TraderCode)

	if row.TGID == "" || row.TraderCode == "" {
		return errors.New("tg_id and trader_code must be provided")
	}
	if row.DecisionTime.IsZero() {
		row.DecisionTime = time.Now().UTC()
	}

	payload := []UserDecisionRow{*row}
	return c.upsertRecords(ctx, "user_trader_decisions", "", payload)
}

// InsertUserTradeExecution 写入单笔交易执行
func (c *Client) InsertUserTradeExecution(ctx context.Context, row *UserTradeExecutionRow) error {
	if c == nil || row == nil {
		return nil
	}

	row.TGID = strings.TrimSpace(row.TGID)
	row.TraderCode = strings.TrimSpace(row.TraderCode)
	row.Action = strings.TrimSpace(row.Action)
	row.Symbol = strings.TrimSpace(row.Symbol)
	row.Side = strings.TrimSpace(row.Side)

	if row.TGID == "" || row.TraderCode == "" {
		return errors.New("tg_id and trader_code must be provided")
	}
	if row.ExecutedAt.IsZero() {
		row.ExecutedAt = time.Now().UTC()
	}

	payload := []UserTradeExecutionRow{*row}
	return c.upsertRecords(ctx, "user_trade_executions", "", payload)
}

// GetUserTraders 获取某个用户的全部交易员配置
func (c *Client) GetUserTraders(ctx context.Context, tgID string) ([]*config.TraderRecord, error) {
	if c == nil {
		return nil, errors.New("supabase client is nil")
	}

	tgID = strings.TrimSpace(tgID)
	if tgID == "" {
		return nil, nil
	}

	params := url.Values{}
	params.Set("tg_id", "eq."+tgID)
	params.Set("select", "trader_code,display_name,ai_model_id,exchange_id,initial_balance,btc_eth_leverage,altcoin_leverage,trading_symbols,custom_prompt,override_base_prompt,system_prompt_template,is_cross_margin,use_coin_pool,use_oi_top,status,extras,created_at,updated_at")

	var rows []struct {
		TraderCode           string          `json:"trader_code"`
		DisplayName          string          `json:"display_name"`
		AIModelID            string          `json:"ai_model_id"`
		ExchangeID           string          `json:"exchange_id"`
		InitialBalance       float64         `json:"initial_balance"`
		BTCETHLeverage       int             `json:"btc_eth_leverage"`
		AltcoinLeverage      int             `json:"altcoin_leverage"`
		TradingSymbols       string          `json:"trading_symbols"`
		CustomPrompt         string          `json:"custom_prompt"`
		OverrideBasePrompt   bool            `json:"override_base_prompt"`
		SystemPromptTemplate string          `json:"system_prompt_template"`
		IsCrossMargin        bool            `json:"is_cross_margin"`
		UseCoinPool          bool            `json:"use_coin_pool"`
		UseOITop             bool            `json:"use_oi_top"`
		Status               string          `json:"status"`
		Extras               json.RawMessage `json:"extras"`
		CreatedAt            *time.Time      `json:"created_at"`
		UpdatedAt            *time.Time      `json:"updated_at"`
	}

	if err := c.fetchRecords(ctx, "user_traders", params, &rows); err != nil {
		return nil, err
	}

	traders := make([]*config.TraderRecord, 0, len(rows))
	for _, row := range rows {
		scanInterval := 3
		useInsideCoins := false

		if len(row.Extras) > 0 {
			var extras map[string]interface{}
			if err := json.Unmarshal(row.Extras, &extras); err == nil {
				if v, ok := extras["scan_interval_minutes"].(float64); ok {
					scanInterval = int(v)
				}
				if v, ok := extras["use_inside_coins"].(bool); ok {
					useInsideCoins = v
				}
				if v, ok := extras["trading_symbols_raw"].(string); ok && strings.TrimSpace(v) != "" {
					row.TradingSymbols = v
				}
			}
		}

		trader := &config.TraderRecord{
			ID:                   row.TraderCode,
			UserID:               tgID,
			Name:                 row.DisplayName,
			AIModelID:            row.AIModelID,
			ExchangeID:           row.ExchangeID,
			InitialBalance:       row.InitialBalance,
			ScanIntervalMinutes:  scanInterval,
			IsRunning:            strings.EqualFold(row.Status, "running"),
			BTCETHLeverage:       row.BTCETHLeverage,
			AltcoinLeverage:      row.AltcoinLeverage,
			TradingSymbols:       row.TradingSymbols,
			UseCoinPool:          row.UseCoinPool,
			UseOITop:             row.UseOITop,
			UseInsideCoins:       useInsideCoins,
			CustomPrompt:         row.CustomPrompt,
			OverrideBasePrompt:   row.OverrideBasePrompt,
			SystemPromptTemplate: row.SystemPromptTemplate,
			IsCrossMargin:        row.IsCrossMargin,
		}

		if row.CreatedAt != nil {
			trader.CreatedAt = *row.CreatedAt
		}
		if row.UpdatedAt != nil {
			trader.UpdatedAt = *row.UpdatedAt
		}

		traders = append(traders, trader)
	}

	return traders, nil
}

func (c *Client) upsertRecords(ctx context.Context, table, conflict string, payload interface{}) error {
	if c == nil {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal supabase payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/rest/v1/%s", c.baseURL, table)
	if conflict != "" {
		endpoint = fmt.Sprintf("%s?on_conflict=%s", endpoint, url.QueryEscape(conflict))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build supabase request: %w", err)
	}

	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "resolution=merge-duplicates,return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call supabase: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase upsert failed: status=%d body=%s", resp.StatusCode, string(responseBody))
	}

	return nil
}

func (c *Client) deleteRows(ctx context.Context, table string, filters map[string]string) error {
	if c == nil {
		return nil
	}
	parts := make([]string, 0, len(filters))
	for key, value := range filters {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=eq.%s", url.QueryEscape(key), url.QueryEscape(value)))
	}
	if len(parts) == 0 {
		return nil
	}

	endpoint := fmt.Sprintf("%s/rest/v1/%s?%s", c.baseURL, table, strings.Join(parts, "&"))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build supabase delete request: %w", err)
	}

	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call supabase delete: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase delete failed: status=%d body=%s", resp.StatusCode, string(responseBody))
	}

	return nil
}

func keyHint(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 6 {
		return "***"
	}
	return fmt.Sprintf("%s***%s", key[:3], key[len(key)-3:])
}

func traderStatus(isRunning bool) string {
	if isRunning {
		return "running"
	}
	return "stopped"
}

func (c *Client) fetchRecords(ctx context.Context, table string, params url.Values, dest interface{}) error {
	if c == nil {
		return errors.New("supabase client is nil")
	}

	endpoint := fmt.Sprintf("%s/rest/v1/%s", c.baseURL, table)
	if params != nil && len(params) > 0 {
		endpoint = fmt.Sprintf("%s?%s", endpoint, params.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build supabase get request: %w", err)
	}

	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call supabase get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase get failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("decode supabase response: %w", err)
	}

	return nil
}

func (c *Client) getUserAPIKeys(ctx context.Context, tgID string) (map[string]string, error) {
	params := url.Values{}
	params.Set("tg_id", "eq."+tgID)
	params.Set("select", "provider,encrypted_key")

	var rows []struct {
		Provider     string `json:"provider"`
		EncryptedKey string `json:"encrypted_key"`
	}

	if err := c.fetchRecords(ctx, "user_api_keys", params, &rows); err != nil {
		return nil, err
	}

	result := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.EncryptedKey == "" {
			continue
		}
		key, err := c.decrypt(row.EncryptedKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt provider %s key: %w", row.Provider, err)
		}
		result[strings.ToLower(row.Provider)] = key
	}
	return result, nil
}

func (c *Client) getHyperliquidKey(ctx context.Context, tgID string) (privateKey string, walletAddr string, err error) {
	params := url.Values{}
	params.Set("tg_id", "eq."+tgID)
	params.Add("key_type", "eq.hyperliquid")
	params.Set("select", "encrypted_private_key,wallet_address")

	var rows []struct {
		EncryptedPrivateKey string `json:"encrypted_private_key"`
		WalletAddress       string `json:"wallet_address"`
	}

	if err := c.fetchRecords(ctx, "user_keys", params, &rows); err != nil {
		return "", "", err
	}

	if len(rows) == 0 {
		return "", "", nil
	}

	if rows[0].EncryptedPrivateKey != "" {
		key, err := c.decrypt(rows[0].EncryptedPrivateKey)
		if err != nil {
			return "", "", fmt.Errorf("decrypt hyperliquid key: %w", err)
		}
		privateKey = key
	}

	walletAddr = rows[0].WalletAddress
	return
}

func (c *Client) GetUserAIModels(ctx context.Context, tgID string) ([]*config.AIModelConfig, error) {
	if c == nil {
		return nil, errors.New("supabase client is nil")
	}

	params := url.Values{}
	params.Set("tg_id", "eq."+tgID)
	params.Set("order", "model_id")

	var rows []struct {
		ModelID         string `json:"model_id"`
		Provider        string `json:"provider"`
		Name            string `json:"name"`
		Enabled         bool   `json:"enabled"`
		CustomAPIURL    string `json:"custom_api_url"`
		CustomModelName string `json:"custom_model_name"`
	}

	if err := c.fetchRecords(ctx, "user_ai_models", params, &rows); err != nil {
		return nil, err
	}

	keyMap, err := c.getUserAPIKeys(ctx, tgID)
	if err != nil {
		return nil, err
	}

	models := make([]*config.AIModelConfig, 0, len(rows))
	for _, row := range rows {
		apiKey := keyMap[strings.ToLower(row.Provider)]
		models = append(models, &config.AIModelConfig{
			ID:              row.ModelID,
			UserID:          tgID,
			Name:            row.Name,
			Provider:        row.Provider,
			Enabled:         row.Enabled,
			APIKey:          apiKey,
			CustomAPIURL:    row.CustomAPIURL,
			CustomModelName: row.CustomModelName,
		})
	}

	return models, nil
}

func (c *Client) GetUserExchanges(ctx context.Context, tgID string) ([]*config.ExchangeConfig, error) {
	if c == nil {
		return nil, errors.New("supabase client is nil")
	}

	params := url.Values{}
	params.Set("tg_id", "eq."+tgID)
	params.Set("order", "exchange_id")

	var rows []struct {
		ExchangeID            string `json:"exchange_id"`
		Name                  string `json:"name"`
		Type                  string `json:"type"`
		Enabled               bool   `json:"enabled"`
		Testnet               bool   `json:"testnet"`
		HyperliquidWalletAddr string `json:"hyperliquid_wallet_addr"`
		AsterUser             string `json:"aster_user"`
		AsterSigner           string `json:"aster_signer"`
		APIKeyEncrypted       string `json:"api_key_encrypted"`
		SecretKeyEncrypted    string `json:"secret_key_encrypted"`
		AsterPrivateEncrypted string `json:"aster_private_key_encrypted"`
	}

	if err := c.fetchRecords(ctx, "user_exchanges", params, &rows); err != nil {
		return nil, err
	}

	hyperKey, hyperWallet, err := c.getHyperliquidKey(ctx, tgID)
	if err != nil {
		return nil, err
	}

	exchanges := make([]*config.ExchangeConfig, 0, len(rows))
	for _, row := range rows {
		apiKey := ""
		secretKey := ""
		asterPrivate := ""

		if row.APIKeyEncrypted != "" {
			if key, err := c.decrypt(row.APIKeyEncrypted); err == nil {
				apiKey = key
			} else {
				return nil, err
			}
		}

		if row.SecretKeyEncrypted != "" {
			if key, err := c.decrypt(row.SecretKeyEncrypted); err == nil {
				secretKey = key
			} else {
				return nil, err
			}
		}

		if row.AsterPrivateEncrypted != "" {
			if key, err := c.decrypt(row.AsterPrivateEncrypted); err == nil {
				asterPrivate = key
			} else {
				return nil, err
			}
		}

		// Hyperliquid 的私钥记录在 user_keys 中
		if strings.EqualFold(row.ExchangeID, "hyperliquid") {
			if hyperKey != "" {
				apiKey = hyperKey
			}
			if hyperWallet != "" && row.HyperliquidWalletAddr == "" {
				row.HyperliquidWalletAddr = hyperWallet
			}
		}

		exchanges = append(exchanges, &config.ExchangeConfig{
			ID:                    row.ExchangeID,
			UserID:                tgID,
			Name:                  row.Name,
			Type:                  row.Type,
			Enabled:               row.Enabled,
			APIKey:                apiKey,
			SecretKey:             secretKey,
			Testnet:               row.Testnet,
			HyperliquidWalletAddr: row.HyperliquidWalletAddr,
			AsterUser:             row.AsterUser,
			AsterSigner:           row.AsterSigner,
			AsterPrivateKey:       asterPrivate,
		})
	}

	return exchanges, nil
}

func (c *Client) encrypt(plain string) (string, error) {
	block, err := aes.NewCipher(c.encryptionKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	// Prepend nonce to ciphertext so it can be used during decryption.
	ciphertext := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return hex.EncodeToString(ciphertext), nil
}

func (c *Client) decrypt(cipherHex string) (string, error) {
	if strings.TrimSpace(cipherHex) == "" {
		return "", nil
	}

	data, err := hex.DecodeString(cipherHex)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(c.encryptionKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce := data[:nonceSize]
	ciphertext := data[nonceSize:]

	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}

	return string(plain), nil
}
