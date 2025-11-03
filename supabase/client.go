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

	if record.TGID == "" || record.ExchangeID == "" {
		return nil
	}

	return c.upsertRecords(ctx, "user_exchanges", "tg_id,exchange_id", []userExchangeRow{record})
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
