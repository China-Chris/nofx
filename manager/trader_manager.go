package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"nofx/config"
	"nofx/logger"
	"nofx/supabase"
	"nofx/trader"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TraderManager 管理多个trader实例
type TraderManager struct {
	traders       map[string]*trader.AutoTrader // key: trader ID
	traderConfigs map[string]*config.TraderRecord
	traderOwners  map[string]string
	aliases       map[string]string
	mu            sync.RWMutex
}

// NewTraderManager 创建trader管理器
func NewTraderManager() *TraderManager {
	return &TraderManager{
		traders:       make(map[string]*trader.AutoTrader),
		traderConfigs: make(map[string]*config.TraderRecord),
		traderOwners:  make(map[string]string),
		aliases:       make(map[string]string),
	}
}

// LoadTraders 从存储加载所有交易员到内存
func (tm *TraderManager) LoadTraders(database *config.Database, supabaseClient *supabase.Client) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// 获取所有用户
	userIDs, err := database.GetAllUsers()
	if err != nil {
		return fmt.Errorf("获取用户列表失败: %w", err)
	}

	log.Printf("📋 发现 %d 个用户，开始加载所有交易员配置...", len(userIDs))

	var allTraders []*config.TraderRecord
	for _, userID := range userIDs {
		var traders []*config.TraderRecord
		var err error

		if supabaseClient != nil && userID != "" && userID != "default" {
			traders, err = supabaseClient.GetUserTraders(context.Background(), userID)
			if err != nil {
				log.Printf("⚠️ 从Supabase获取用户 %s 的交易员失败: %v，回退到本地数据库", userID, err)
				traders = nil
			}
		}

		if len(traders) == 0 {
			traders, err = database.GetTraders(userID)
			if err != nil {
				log.Printf("⚠️ 获取用户 %s 的交易员失败: %v", userID, err)
				continue
			}
		}

		log.Printf("📋 用户 %s: %d 个交易员", userID, len(traders))
		allTraders = append(allTraders, traders...)
	}

	log.Printf("📋 总共加载 %d 个交易员配置", len(allTraders))

	// 获取系统配置（不包含信号源，信号源现在为用户级别）
	maxDailyLossStr, _ := database.GetSystemConfig("max_daily_loss")
	maxDrawdownStr, _ := database.GetSystemConfig("max_drawdown")
	stopTradingMinutesStr, _ := database.GetSystemConfig("stop_trading_minutes")
	defaultCoinsStr, _ := database.GetSystemConfig("default_coins")

	// 解析配置
	maxDailyLoss := 10.0 // 默认值
	if val, err := strconv.ParseFloat(maxDailyLossStr, 64); err == nil {
		maxDailyLoss = val
	}

	maxDrawdown := 20.0 // 默认值
	if val, err := strconv.ParseFloat(maxDrawdownStr, 64); err == nil {
		maxDrawdown = val
	}

	stopTradingMinutes := 60 // 默认值
	if val, err := strconv.Atoi(stopTradingMinutesStr); err == nil {
		stopTradingMinutes = val
	}

	// 解析默认币种列表
	var defaultCoins []string
	if defaultCoinsStr != "" {
		if err := json.Unmarshal([]byte(defaultCoinsStr), &defaultCoins); err != nil {
			log.Printf("⚠️ 解析默认币种配置失败: %v，使用空列表", err)
			defaultCoins = []string{}
		}
	}

	// 为每个交易员获取AI模型和交易所配置
	for _, traderCfg := range allTraders {
		tm.registerTraderMetadata(traderCfg)

		// 获取AI模型配置（使用交易员所属的用户ID）
		aiModels, err := fetchAIModels(context.Background(), supabaseClient, database, traderCfg.UserID)
		if err != nil {
			log.Printf("⚠️  获取AI模型配置失败: %v", err)
			continue
		}

		var aiModelCfg *config.AIModelConfig
		// 优先精确匹配 model.ID（新版逻辑）
		for _, model := range aiModels {
			if model.ID == traderCfg.AIModelID {
				aiModelCfg = model
				break
			}
		}
		// 如果没有精确匹配，尝试匹配 provider（兼容旧数据）
		if aiModelCfg == nil {
			for _, model := range aiModels {
				if model.Provider == traderCfg.AIModelID {
					aiModelCfg = model
					log.Printf("⚠️  交易员 %s 使用旧版 provider 匹配: %s -> %s", traderCfg.Name, traderCfg.AIModelID, model.ID)
					break
				}
			}
		}

		if aiModelCfg == nil {
			log.Printf("⚠️  交易员 %s 的AI模型 %s 不存在，跳过", traderCfg.Name, traderCfg.AIModelID)
			continue
		}

		if !aiModelCfg.Enabled {
			log.Printf("⚠️  交易员 %s 的AI模型 %s 未启用，跳过", traderCfg.Name, traderCfg.AIModelID)
			continue
		}

		// 获取交易所配置（使用交易员所属的用户ID）
		exchanges, err := fetchExchanges(context.Background(), supabaseClient, database, traderCfg.UserID)
		if err != nil {
			log.Printf("⚠️  获取交易所配置失败: %v", err)
			continue
		}

		var exchangeCfg *config.ExchangeConfig
		for _, exchange := range exchanges {
			if exchange.ID == traderCfg.ExchangeID {
				exchangeCfg = exchange
				break
			}
		}

		if exchangeCfg == nil {
			log.Printf("⚠️  交易员 %s 的交易所 %s 不存在，跳过", traderCfg.Name, traderCfg.ExchangeID)
			continue
		}

		if !exchangeCfg.Enabled {
			log.Printf("⚠️  交易员 %s 的交易所 %s 未启用，跳过", traderCfg.Name, traderCfg.ExchangeID)
			continue
		}

		// 获取用户信号源配置
		var coinPoolURL, oiTopURL string
		if userSignalSource, err := database.GetUserSignalSource(traderCfg.UserID); err == nil {
			coinPoolURL = userSignalSource.CoinPoolURL
			oiTopURL = userSignalSource.OITopURL
		} else {
			// 如果用户没有配置信号源，使用空字符串
			log.Printf("🔍 用户 %s 暂未配置信号源", traderCfg.UserID)
		}

		// 添加到TraderManager
		err = tm.addTraderFromDB(traderCfg, aiModelCfg, exchangeCfg, coinPoolURL, oiTopURL, maxDailyLoss, maxDrawdown, stopTradingMinutes, defaultCoins, supabaseClient)
		if err != nil {
			log.Printf("❌ 添加交易员 %s 失败: %v", traderCfg.Name, err)
			continue
		}
	}

	log.Printf("✓ 成功加载 %d 个交易员到内存", len(tm.traders))
	return nil
}

// addTraderFromConfig 内部方法：从配置添加交易员（不加锁，因为调用方已加锁）
func (tm *TraderManager) addTraderFromDB(traderCfg *config.TraderRecord, aiModelCfg *config.AIModelConfig, exchangeCfg *config.ExchangeConfig, coinPoolURL, oiTopURL string, maxDailyLoss, maxDrawdown float64, stopTradingMinutes int, defaultCoins []string, supabaseClient *supabase.Client) error {
	if _, exists := tm.traders[traderCfg.ID]; exists {
		return fmt.Errorf("trader ID '%s' 已存在", traderCfg.ID)
	}

	// 处理交易币种列表
	var tradingCoins []string
	if traderCfg.TradingSymbols != "" {
		// 解析逗号分隔的交易币种列表
		symbols := strings.Split(traderCfg.TradingSymbols, ",")
		for _, symbol := range symbols {
			symbol = strings.TrimSpace(symbol)
			if symbol != "" {
				tradingCoins = append(tradingCoins, symbol)
			}
		}
	}

	// 如果没有指定交易币种，使用默认币种
	if len(tradingCoins) == 0 {
		tradingCoins = defaultCoins
	}

	// 根据交易员配置决定是否使用信号源
	var effectiveCoinPoolURL string
	if traderCfg.UseCoinPool && coinPoolURL != "" {
		effectiveCoinPoolURL = coinPoolURL
		log.Printf("✓ 交易员 %s 启用 COIN POOL 信号源: %s", traderCfg.Name, coinPoolURL)
	}

	// 构建AutoTraderConfig
	traderConfig := trader.AutoTraderConfig{
		ID:                    traderCfg.ID,
		Name:                  traderCfg.Name,
		AIModel:               aiModelCfg.Provider, // 使用provider作为模型标识
		Exchange:              exchangeCfg.ID,      // 使用exchange ID
		BinanceAPIKey:         "",
		BinanceSecretKey:      "",
		HyperliquidPrivateKey: "",
		HyperliquidTestnet:    exchangeCfg.Testnet,
		CoinPoolAPIURL:        effectiveCoinPoolURL,
		UseQwen:               aiModelCfg.Provider == "qwen",
		DeepSeekKey:           "",
		QwenKey:               "",
		CustomAPIURL:          aiModelCfg.CustomAPIURL,    // 自定义API URL
		CustomModelName:       aiModelCfg.CustomModelName, // 自定义模型名称
		ScanInterval:          time.Duration(traderCfg.ScanIntervalMinutes) * time.Minute,
		InitialBalance:        traderCfg.InitialBalance,
		BTCETHLeverage:        traderCfg.BTCETHLeverage,
		AltcoinLeverage:       traderCfg.AltcoinLeverage,
		MaxDailyLoss:          maxDailyLoss,
		MaxDrawdown:           maxDrawdown,
		StopTradingTime:       time.Duration(stopTradingMinutes) * time.Minute,
		IsCrossMargin:         traderCfg.IsCrossMargin,
		DefaultCoins:          defaultCoins,
		TradingCoins:          tradingCoins,
		SystemPromptTemplate:  traderCfg.SystemPromptTemplate, // 系统提示词模板
	}

	// 根据交易所类型设置API密钥
	if exchangeCfg.ID == "binance" {
		traderConfig.BinanceAPIKey = exchangeCfg.APIKey
		traderConfig.BinanceSecretKey = exchangeCfg.SecretKey
	} else if exchangeCfg.ID == "hyperliquid" {
		traderConfig.HyperliquidPrivateKey = exchangeCfg.APIKey // hyperliquid用APIKey存储private key
		traderConfig.HyperliquidWalletAddr = exchangeCfg.HyperliquidWalletAddr
	} else if exchangeCfg.ID == "aster" {
		traderConfig.AsterUser = exchangeCfg.AsterUser
		traderConfig.AsterSigner = exchangeCfg.AsterSigner
		traderConfig.AsterPrivateKey = exchangeCfg.AsterPrivateKey
	}

	// 根据AI模型设置API密钥
	if aiModelCfg.Provider == "qwen" {
		traderConfig.QwenKey = aiModelCfg.APIKey
	} else if aiModelCfg.Provider == "deepseek" {
		traderConfig.DeepSeekKey = aiModelCfg.APIKey
	}

	// 创建trader实例
	at, err := trader.NewAutoTrader(traderConfig)
	if err != nil {
		return fmt.Errorf("创建trader失败: %w", err)
	}

	tm.attachDecisionReporter(at, traderCfg, supabaseClient)

	// 设置自定义prompt（如果有）
	if traderCfg.CustomPrompt != "" {
		at.SetCustomPrompt(traderCfg.CustomPrompt)
		at.SetOverrideBasePrompt(traderCfg.OverrideBasePrompt)
		if traderCfg.OverrideBasePrompt {
			log.Printf("✓ 已设置自定义交易策略prompt (覆盖基础prompt)")
		} else {
			log.Printf("✓ 已设置自定义交易策略prompt (补充基础prompt)")
		}
	}

	tm.traders[traderCfg.ID] = at
	log.Printf("✓ Trader '%s' (%s + %s) 已加载到内存", traderCfg.Name, aiModelCfg.Provider, exchangeCfg.ID)
	return nil
}

// AddTrader 从数据库配置添加trader (移除旧版兼容性)

// AddTraderFromDB 从数据库配置添加trader
func (tm *TraderManager) AddTraderFromDB(traderCfg *config.TraderRecord, aiModelCfg *config.AIModelConfig, exchangeCfg *config.ExchangeConfig, coinPoolURL, oiTopURL string, maxDailyLoss, maxDrawdown float64, stopTradingMinutes int, defaultCoins []string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if _, exists := tm.traders[traderCfg.ID]; exists {
		return fmt.Errorf("trader ID '%s' 已存在", traderCfg.ID)
	}

	// 处理交易币种列表
	var tradingCoins []string
	if traderCfg.TradingSymbols != "" {
		// 解析逗号分隔的交易币种列表
		symbols := strings.Split(traderCfg.TradingSymbols, ",")
		for _, symbol := range symbols {
			symbol = strings.TrimSpace(symbol)
			if symbol != "" {
				tradingCoins = append(tradingCoins, symbol)
			}
		}
	}

	// 如果没有指定交易币种，使用默认币种
	if len(tradingCoins) == 0 {
		tradingCoins = defaultCoins
	}

	// 根据交易员配置决定是否使用信号源
	var effectiveCoinPoolURL string
	if traderCfg.UseCoinPool && coinPoolURL != "" {
		effectiveCoinPoolURL = coinPoolURL
		log.Printf("✓ 交易员 %s 启用 COIN POOL 信号源: %s", traderCfg.Name, coinPoolURL)
	}

	// 构建AutoTraderConfig
	traderConfig := trader.AutoTraderConfig{
		ID:                    traderCfg.ID,
		Name:                  traderCfg.Name,
		AIModel:               aiModelCfg.Provider, // 使用provider作为模型标识
		Exchange:              exchangeCfg.ID,      // 使用exchange ID
		BinanceAPIKey:         "",
		BinanceSecretKey:      "",
		HyperliquidPrivateKey: "",
		HyperliquidTestnet:    exchangeCfg.Testnet,
		CoinPoolAPIURL:        effectiveCoinPoolURL,
		UseQwen:               aiModelCfg.Provider == "qwen",
		DeepSeekKey:           "",
		QwenKey:               "",
		CustomAPIURL:          aiModelCfg.CustomAPIURL,    // 自定义API URL
		CustomModelName:       aiModelCfg.CustomModelName, // 自定义模型名称
		ScanInterval:          time.Duration(traderCfg.ScanIntervalMinutes) * time.Minute,
		InitialBalance:        traderCfg.InitialBalance,
		BTCETHLeverage:        traderCfg.BTCETHLeverage,
		AltcoinLeverage:       traderCfg.AltcoinLeverage,
		MaxDailyLoss:          maxDailyLoss,
		MaxDrawdown:           maxDrawdown,
		StopTradingTime:       time.Duration(stopTradingMinutes) * time.Minute,
		IsCrossMargin:         traderCfg.IsCrossMargin,
		DefaultCoins:          defaultCoins,
		TradingCoins:          tradingCoins,
	}

	// 根据交易所类型设置API密钥
	if exchangeCfg.ID == "binance" {
		traderConfig.BinanceAPIKey = exchangeCfg.APIKey
		traderConfig.BinanceSecretKey = exchangeCfg.SecretKey
	} else if exchangeCfg.ID == "hyperliquid" {
		traderConfig.HyperliquidPrivateKey = exchangeCfg.APIKey // hyperliquid用APIKey存储private key
		traderConfig.HyperliquidWalletAddr = exchangeCfg.HyperliquidWalletAddr
	} else if exchangeCfg.ID == "aster" {
		traderConfig.AsterUser = exchangeCfg.AsterUser
		traderConfig.AsterSigner = exchangeCfg.AsterSigner
		traderConfig.AsterPrivateKey = exchangeCfg.AsterPrivateKey
	}

	// 根据AI模型设置API密钥
	if aiModelCfg.Provider == "qwen" {
		traderConfig.QwenKey = aiModelCfg.APIKey
	} else if aiModelCfg.Provider == "deepseek" {
		traderConfig.DeepSeekKey = aiModelCfg.APIKey
	}

	// 创建trader实例
	at, err := trader.NewAutoTrader(traderConfig)
	if err != nil {
		return fmt.Errorf("创建trader失败: %w", err)
	}

	// 设置自定义prompt（如果有）
	if traderCfg.CustomPrompt != "" {
		at.SetCustomPrompt(traderCfg.CustomPrompt)
		at.SetOverrideBasePrompt(traderCfg.OverrideBasePrompt)
		if traderCfg.OverrideBasePrompt {
			log.Printf("✓ 已设置自定义交易策略prompt (覆盖基础prompt)")
		} else {
			log.Printf("✓ 已设置自定义交易策略prompt (补充基础prompt)")
		}
	}

	tm.traders[traderCfg.ID] = at
	log.Printf("✓ Trader '%s' (%s + %s) 已添加", traderCfg.Name, aiModelCfg.Provider, exchangeCfg.ID)
	return nil
}

// GetTraderWithID 同时返回交易员实例与规范化后的ID
func (tm *TraderManager) GetTraderWithID(identifier string) (*trader.AutoTrader, string, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	canonicalID, ok := tm.resolveTraderIDLocked(identifier)
	if !ok {
		return nil, "", fmt.Errorf("trader ID '%s' 不存在", identifier)
	}

	at := tm.traders[canonicalID]
	if at == nil {
		return nil, "", fmt.Errorf("trader ID '%s' 不存在", identifier)
	}
	return at, canonicalID, nil
}

// GetTrader 获取指定ID的trader
func (tm *TraderManager) GetTrader(id string) (*trader.AutoTrader, error) {
	at, _, err := tm.GetTraderWithID(id)
	return at, err
}

// GetAllTraders 获取所有trader
func (tm *TraderManager) GetAllTraders() map[string]*trader.AutoTrader {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	result := make(map[string]*trader.AutoTrader)
	for id, t := range tm.traders {
		result[id] = t
	}
	return result
}

// GetTraderIDs 获取所有trader ID列表
func (tm *TraderManager) GetTraderIDs() []string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	ids := make([]string, 0, len(tm.traders))
	for id := range tm.traders {
		ids = append(ids, id)
	}
	return ids
}

// GetTraderOwner 返回交易员所属用户
func (tm *TraderManager) GetTraderOwner(id string) string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.traderOwners[id]
}

// GetTraderConfig 返回交易员配置副本
func (tm *TraderManager) GetTraderConfig(id string) *config.TraderRecord {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	cfg, ok := tm.traderConfigs[id]
	if !ok || cfg == nil {
		return nil
	}
	copyCfg := *cfg
	return &copyCfg
}

// SetTraderRunning 更新交易员运行状态（仅影响内存缓存）
func (tm *TraderManager) SetTraderRunning(id string, running bool) {
	tm.UpdateTraderConfig(id, func(cfg *config.TraderRecord) {
		cfg.IsRunning = running
	})
}

// ResolveTraderID 将传入标识转换为真实交易员ID
func (tm *TraderManager) ResolveTraderID(identifier string) (string, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if canonicalID, ok := tm.resolveTraderIDLocked(identifier); ok {
		return canonicalID, nil
	}
	return "", fmt.Errorf("trader ID '%s' 不存在", identifier)
}

// UpdateTraderConfig 在保持互斥的情况下更新缓存中的交易员配置
func (tm *TraderManager) UpdateTraderConfig(id string, updater func(cfg *config.TraderRecord)) {
	if updater == nil {
		return
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if cfg, ok := tm.traderConfigs[id]; ok && cfg != nil {
		updater(cfg)
	}
}

// StartAll 启动所有trader
func (tm *TraderManager) StartAll() {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	log.Println("🚀 启动所有Trader...")
	for id, t := range tm.traders {
		go func(traderID string, at *trader.AutoTrader) {
			log.Printf("▶️  启动 %s...", at.GetName())
			if err := at.Run(); err != nil {
				log.Printf("❌ %s 运行错误: %v", at.GetName(), err)
			}
		}(id, t)
	}
}

// StopAll 停止所有trader
func (tm *TraderManager) StopAll() {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	log.Println("⏹  停止所有Trader...")
	for _, t := range tm.traders {
		t.Stop()
	}
}

// GetComparisonData 获取对比数据
func (tm *TraderManager) GetComparisonData() (map[string]interface{}, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	comparison := make(map[string]interface{})
	traders := make([]map[string]interface{}, 0, len(tm.traders))

	for _, t := range tm.traders {
		account, err := t.GetAccountInfo()
		if err != nil {
			continue
		}

		status := t.GetStatus()

		traders = append(traders, map[string]interface{}{
			"trader_id":       t.GetID(),
			"trader_name":     t.GetName(),
			"ai_model":        t.GetAIModel(),
			"exchange":        t.GetExchange(),
			"total_equity":    account["total_equity"],
			"total_pnl":       account["total_pnl"],
			"total_pnl_pct":   account["total_pnl_pct"],
			"position_count":  account["position_count"],
			"margin_used_pct": account["margin_used_pct"],
			"call_count":      status["call_count"],
			"is_running":      status["is_running"],
		})
	}

	comparison["traders"] = traders
	comparison["count"] = len(traders)

	return comparison, nil
}

// GetCompetitionData 获取竞赛数据（全平台所有交易员）
func (tm *TraderManager) GetCompetitionData() (map[string]interface{}, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	comparison := make(map[string]interface{})
	traders := make([]map[string]interface{}, 0)

	// 获取全平台所有交易员
	for _, t := range tm.traders {
		account, err := t.GetAccountInfo()
		status := t.GetStatus()

		var traderData map[string]interface{}

		if err != nil {
			// 如果获取账户信息失败，使用默认值但仍然显示交易员
			log.Printf("⚠️ 获取交易员 %s 账户信息失败: %v", t.GetID(), err)
			traderData = map[string]interface{}{
				"trader_id":       t.GetID(),
				"trader_name":     t.GetName(),
				"ai_model":        t.GetAIModel(),
				"exchange":        t.GetExchange(),
				"total_equity":    0.0,
				"total_pnl":       0.0,
				"total_pnl_pct":   0.0,
				"position_count":  0,
				"margin_used_pct": 0.0,
				"is_running":      status["is_running"],
				"error":           "账户数据获取失败",
			}
		} else {
			// 正常情况下使用真实账户数据
			traderData = map[string]interface{}{
				"trader_id":       t.GetID(),
				"trader_name":     t.GetName(),
				"ai_model":        t.GetAIModel(),
				"exchange":        t.GetExchange(),
				"total_equity":    account["total_equity"],
				"total_pnl":       account["total_pnl"],
				"total_pnl_pct":   account["total_pnl_pct"],
				"position_count":  account["position_count"],
				"margin_used_pct": account["margin_used_pct"],
				"is_running":      status["is_running"],
			}
		}

		traders = append(traders, traderData)
	}
	comparison["traders"] = traders
	comparison["count"] = len(traders)

	return comparison, nil
}

func (tm *TraderManager) resolveTraderIDLocked(identifier string) (string, bool) {
	if identifier == "" {
		return "", false
	}

	if _, exists := tm.traders[identifier]; exists {
		return identifier, true
	}

	key := strings.ToLower(strings.TrimSpace(identifier))
	if key != "" {
		if id, ok := tm.aliases[key]; ok {
			if _, exists := tm.traders[id]; exists {
				return id, true
			}
		}
	}

	if tgID := extractTGIDFromSlug(identifier); tgID != "" {
		var candidate string
		for id, owner := range tm.traderOwners {
			if owner != tgID {
				continue
			}
			if _, exists := tm.traders[id]; !exists {
				continue
			}

			nameKey := strings.ToLower(strings.TrimSpace(tm.traders[id].GetName()))
			if nameKey == key || strings.Contains(nameKey, key) {
				return id, true
			}
			if candidate == "" {
				candidate = id
			}
		}
		if candidate != "" {
			return candidate, true
		}
	}

	return "", false
}

func (tm *TraderManager) registerTraderMetadata(traderCfg *config.TraderRecord) {
	if traderCfg == nil {
		return
	}

	if tm.aliases == nil {
		tm.aliases = make(map[string]string)
	}
	if tm.traderOwners == nil {
		tm.traderOwners = make(map[string]string)
	}
	if tm.traderConfigs == nil {
		tm.traderConfigs = make(map[string]*config.TraderRecord)
	}

	idKey := strings.ToLower(strings.TrimSpace(traderCfg.ID))
	if idKey != "" {
		tm.aliases[idKey] = traderCfg.ID
	}

	nameKey := strings.ToLower(strings.TrimSpace(traderCfg.Name))
	if nameKey != "" {
		tm.aliases[nameKey] = traderCfg.ID
	}

	tm.traderOwners[traderCfg.ID] = traderCfg.UserID

	copyCfg := *traderCfg
	tm.traderConfigs[traderCfg.ID] = &copyCfg
}

func extractTGIDFromSlug(slug string) string {
	parts := strings.Split(slug, "-")
	if len(parts) < 2 {
		return ""
	}
	candidate := parts[1]
	if candidate == "" {
		return ""
	}
	for _, ch := range candidate {
		if ch < '0' || ch > '9' {
			return ""
		}
	}
	return candidate
}

func (tm *TraderManager) attachDecisionReporter(at *trader.AutoTrader, traderCfg *config.TraderRecord, supabaseClient *supabase.Client) {
	if at == nil || traderCfg == nil || supabaseClient == nil {
		return
	}

	ownerID := strings.TrimSpace(traderCfg.UserID)
	if ownerID == "" || ownerID == "default" {
		return
	}

	traderID := strings.TrimSpace(traderCfg.ID)
	if traderID == "" {
		return
	}

	at.SetDecisionReporter(func(record *logger.DecisionRecord) {
		if record == nil {
			return
		}

		summary, executionLog := buildDecisionSummary(record)
		rawPayload, err := json.Marshal(record)
		if err != nil {
			log.Printf("⚠️  序列化决策记录失败 (user=%s trader=%s cycle=%d): %v", ownerID, traderID, record.CycleNumber, err)
			return
		}

		row := supabase.UserDecisionRow{
			TGID:            ownerID,
			TraderCode:      traderID,
			DecisionCycle:   record.CycleNumber,
			DecisionTime:    record.Timestamp,
			DecisionSummary: summary,
			ExecutionLog:    executionLog,
			Success:         record.Success,
			ErrorMessage:    record.ErrorMessage,
			RawPayload:      rawPayload,
		}

		if err := supabaseClient.InsertUserTraderDecision(context.Background(), &row); err != nil {
			log.Printf("❌ 同步决策记录到Supabase失败 (user=%s trader=%s cycle=%d): %v", ownerID, traderID, record.CycleNumber, err)
		}
	})

	at.SetExecutionReporter(func(action *logger.DecisionAction, record *logger.DecisionRecord) {
		if action == nil {
			return
		}

		side := deriveActionSide(action.Action)
		payload, err := json.Marshal(action)
		if err != nil {
			log.Printf("⚠️  序列化交易动作失败 (user=%s trader=%s action=%s): %v", ownerID, traderID, action.Action, err)
			return
		}

		cycle := 0
		if record != nil {
			cycle = record.CycleNumber
		}

		executedAt := action.Timestamp
		if executedAt.IsZero() {
			executedAt = time.Now().UTC()
		}

		row := supabase.UserTradeExecutionRow{
			TGID:          ownerID,
			TraderCode:    traderID,
			DecisionCycle: cycle,
			Action:        action.Action,
			Symbol:        strings.ToUpper(action.Symbol),
			Side:          side,
			Quantity:      action.Quantity,
			Price:         action.Price,
			Leverage:      action.Leverage,
			Success:       action.Success,
			ErrorMessage:  action.Error,
			RawPayload:    payload,
			ExecutedAt:    executedAt,
		}

		if err := supabaseClient.InsertUserTradeExecution(context.Background(), &row); err != nil {
			log.Printf("❌ 同步交易执行到Supabase失败 (user=%s trader=%s action=%s): %v", ownerID, traderID, action.Action, err)
		}
	})
}

func buildDecisionSummary(record *logger.DecisionRecord) (string, string) {
	status := "成功"
	if !record.Success {
		status = "失败"
	}

	actions := make([]string, 0, len(record.Decisions))
	for _, action := range record.Decisions {
		label := fmt.Sprintf("%s %s", strings.ToUpper(action.Symbol), action.Action)
		if action.Success {
			label = label + "✓"
		} else if action.Error != "" {
			label = label + "✗"
		}
		actions = append(actions, label)
	}

	actionSummary := strings.Join(actions, ", ")
	if actionSummary == "" && len(record.ExecutionLog) > 0 {
		actionSummary = record.ExecutionLog[len(record.ExecutionLog)-1]
	}
	if actionSummary == "" {
		actionSummary = "无执行动作"
	}

	summary := fmt.Sprintf("周期 #%d %s - %s", record.CycleNumber, status, actionSummary)
	executionLog := strings.Join(record.ExecutionLog, "\n")
	return summary, executionLog
}

func deriveActionSide(action string) string {
	switch strings.ToLower(action) {
	case "open_long", "close_long":
		return "long"
	case "open_short", "close_short":
		return "short"
	default:
		return "neutral"
	}
}

// isUserTrader 检查trader是否属于指定用户
func isUserTrader(traderID, userID string) bool {
	// trader ID格式: userID_traderName 或 randomUUID_modelName
	// 为了兼容性，我们检查前缀
	if len(traderID) >= len(userID) && traderID[:len(userID)] == userID {
		return true
	}
	// 对于老的default用户，所有没有明确用户前缀的都属于default
	if userID == "default" && !containsUserPrefix(traderID) {
		return true
	}
	return false
}

// containsUserPrefix 检查trader ID是否包含用户前缀
func containsUserPrefix(traderID string) bool {
	// 检查是否包含邮箱格式的前缀（user@example.com_traderName）
	for i, ch := range traderID {
		if ch == '@' {
			// 找到@符号，说明可能是email前缀
			return true
		}
		if ch == '_' && i > 0 {
			// 找到下划线但前面没有@，可能是UUID或其他格式
			break
		}
	}
	return false
}

func fetchAIModels(ctx context.Context, supabaseClient *supabase.Client, database *config.Database, userID string) ([]*config.AIModelConfig, error) {
	if supabaseClient != nil && userID != "" && userID != "default" {
		models, err := supabaseClient.GetUserAIModels(ctx, userID)
		if err == nil {
			return models, nil
		}
		log.Printf("⚠️  从Supabase获取用户 %s 的AI模型失败: %v，回退到本地数据库", userID, err)
	}
	return database.GetAIModels(userID)
}

func fetchExchanges(ctx context.Context, supabaseClient *supabase.Client, database *config.Database, userID string) ([]*config.ExchangeConfig, error) {
	if supabaseClient != nil && userID != "" && userID != "default" {
		exchanges, err := supabaseClient.GetUserExchanges(ctx, userID)
		if err == nil {
			return exchanges, nil
		}
		log.Printf("⚠️  从Supabase获取用户 %s 的交易所配置失败: %v，回退到本地数据库", userID, err)
	}
	return database.GetExchanges(userID)
}

// LoadUserTraders 为特定用户加载交易员到内存
func (tm *TraderManager) LoadUserTraders(database *config.Database, supabaseClient *supabase.Client, userID string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	var traders []*config.TraderRecord
	var err error

	if supabaseClient != nil && userID != "" && userID != "default" {
		traders, err = supabaseClient.GetUserTraders(context.Background(), userID)
		if err != nil {
			log.Printf("⚠️ 从Supabase获取用户 %s 的交易员失败: %v，回退到本地数据库", userID, err)
			traders = nil
		}
	}

	if len(traders) == 0 {
		traders, err = database.GetTraders(userID)
		if err != nil {
			return fmt.Errorf("获取用户 %s 的交易员列表失败: %w", userID, err)
		}
	}

	log.Printf("📋 为用户 %s 加载交易员配置: %d 个", userID, len(traders))

	// 获取系统配置（不包含信号源，信号源现在为用户级别）
	maxDailyLossStr, _ := database.GetSystemConfig("max_daily_loss")
	maxDrawdownStr, _ := database.GetSystemConfig("max_drawdown")
	stopTradingMinutesStr, _ := database.GetSystemConfig("stop_trading_minutes")
	defaultCoinsStr, _ := database.GetSystemConfig("default_coins")

	// 获取用户信号源配置
	var coinPoolURL, oiTopURL string
	if userSignalSource, err := database.GetUserSignalSource(userID); err == nil {
		coinPoolURL = userSignalSource.CoinPoolURL
		oiTopURL = userSignalSource.OITopURL
		log.Printf("📡 加载用户 %s 的信号源配置: COIN POOL=%s, OI TOP=%s", userID, coinPoolURL, oiTopURL)
	} else {
		log.Printf("🔍 用户 %s 暂未配置信号源", userID)
	}

	// 解析配置
	maxDailyLoss := 10.0 // 默认值
	if val, err := strconv.ParseFloat(maxDailyLossStr, 64); err == nil {
		maxDailyLoss = val
	}

	maxDrawdown := 20.0 // 默认值
	if val, err := strconv.ParseFloat(maxDrawdownStr, 64); err == nil {
		maxDrawdown = val
	}

	stopTradingMinutes := 60 // 默认值
	if val, err := strconv.Atoi(stopTradingMinutesStr); err == nil {
		stopTradingMinutes = val
	}

	// 解析默认币种列表
	var defaultCoins []string
	if defaultCoinsStr != "" {
		if err := json.Unmarshal([]byte(defaultCoinsStr), &defaultCoins); err != nil {
			log.Printf("⚠️ 解析默认币种配置失败: %v，使用空列表", err)
			defaultCoins = []string{}
		}
	}

	// 为每个交易员获取AI模型和交易所配置
	for _, traderCfg := range traders {
		tm.registerTraderMetadata(traderCfg)

		// 检查是否已经加载过这个交易员
		if _, exists := tm.traders[traderCfg.ID]; exists {
			log.Printf("⚠️ 交易员 %s 已经加载，跳过", traderCfg.Name)
			continue
		}

		// 获取AI模型配置（使用该用户的配置）
		aiModels, err := fetchAIModels(context.Background(), supabaseClient, database, userID)
		if err != nil {
			log.Printf("⚠️ 获取用户 %s 的AI模型配置失败: %v", userID, err)
			continue
		}

		var aiModelCfg *config.AIModelConfig
		// 优先精确匹配 model.ID（新版逻辑）
		for _, model := range aiModels {
			if model.ID == traderCfg.AIModelID {
				aiModelCfg = model
				break
			}
		}
		// 如果没有精确匹配，尝试匹配 provider（兼容旧数据）
		if aiModelCfg == nil {
			for _, model := range aiModels {
				if model.Provider == traderCfg.AIModelID {
					aiModelCfg = model
					log.Printf("⚠️  交易员 %s 使用旧版 provider 匹配: %s -> %s", traderCfg.Name, traderCfg.AIModelID, model.ID)
					break
				}
			}
		}

		if aiModelCfg == nil {
			log.Printf("⚠️ 交易员 %s 的AI模型 %s 不存在，跳过", traderCfg.Name, traderCfg.AIModelID)
			continue
		}

		if !aiModelCfg.Enabled {
			log.Printf("⚠️ 交易员 %s 的AI模型 %s 未启用，跳过", traderCfg.Name, traderCfg.AIModelID)
			continue
		}

		// 获取交易所配置（使用该用户的配置）
		exchanges, err := fetchExchanges(context.Background(), supabaseClient, database, userID)
		if err != nil {
			log.Printf("⚠️ 获取用户 %s 的交易所配置失败: %v", userID, err)
			continue
		}

		var exchangeCfg *config.ExchangeConfig
		for _, exchange := range exchanges {
			if exchange.ID == traderCfg.ExchangeID {
				exchangeCfg = exchange
				break
			}
		}

		if exchangeCfg == nil {
			log.Printf("⚠️ 交易员 %s 的交易所 %s 不存在，跳过", traderCfg.Name, traderCfg.ExchangeID)
			continue
		}

		if !exchangeCfg.Enabled {
			log.Printf("⚠️ 交易员 %s 的交易所 %s 未启用，跳过", traderCfg.Name, traderCfg.ExchangeID)
			continue
		}

		// 使用现有的方法加载交易员
		err = tm.loadSingleTrader(traderCfg, aiModelCfg, exchangeCfg, coinPoolURL, oiTopURL, maxDailyLoss, maxDrawdown, stopTradingMinutes, defaultCoins, supabaseClient)
		if err != nil {
			log.Printf("⚠️ 加载交易员 %s 失败: %v", traderCfg.Name, err)
		}
	}

	return nil
}

// loadSingleTrader 加载单个交易员（从现有代码提取的公共逻辑）
func (tm *TraderManager) loadSingleTrader(traderCfg *config.TraderRecord, aiModelCfg *config.AIModelConfig, exchangeCfg *config.ExchangeConfig, coinPoolURL, oiTopURL string, maxDailyLoss, maxDrawdown float64, stopTradingMinutes int, defaultCoins []string, supabaseClient *supabase.Client) error {
	tm.registerTraderMetadata(traderCfg)

	// 处理交易币种列表
	var tradingCoins []string
	if traderCfg.TradingSymbols != "" {
		// 解析逗号分隔的交易币种列表
		symbols := strings.Split(traderCfg.TradingSymbols, ",")
		for _, symbol := range symbols {
			symbol = strings.TrimSpace(symbol)
			if symbol != "" {
				tradingCoins = append(tradingCoins, symbol)
			}
		}
	}

	// 如果没有指定交易币种，使用默认币种
	if len(tradingCoins) == 0 {
		tradingCoins = defaultCoins
	}

	// 根据交易员配置决定是否使用信号源
	var effectiveCoinPoolURL string
	if traderCfg.UseCoinPool && coinPoolURL != "" {
		effectiveCoinPoolURL = coinPoolURL
		log.Printf("✓ 交易员 %s 启用 COIN POOL 信号源: %s", traderCfg.Name, coinPoolURL)
	}

	// 构建AutoTraderConfig
	traderConfig := trader.AutoTraderConfig{
		ID:                   traderCfg.ID,
		Name:                 traderCfg.Name,
		AIModel:              aiModelCfg.Provider, // 使用provider作为模型标识
		Exchange:             exchangeCfg.ID,      // 使用exchange ID
		InitialBalance:       traderCfg.InitialBalance,
		BTCETHLeverage:       traderCfg.BTCETHLeverage,
		AltcoinLeverage:      traderCfg.AltcoinLeverage,
		ScanInterval:         time.Duration(traderCfg.ScanIntervalMinutes) * time.Minute,
		CoinPoolAPIURL:       effectiveCoinPoolURL,
		CustomAPIURL:         aiModelCfg.CustomAPIURL,    // 自定义API URL
		CustomModelName:      aiModelCfg.CustomModelName, // 自定义模型名称
		UseQwen:              aiModelCfg.Provider == "qwen",
		MaxDailyLoss:         maxDailyLoss,
		MaxDrawdown:          maxDrawdown,
		StopTradingTime:      time.Duration(stopTradingMinutes) * time.Minute,
		IsCrossMargin:        traderCfg.IsCrossMargin,
		DefaultCoins:         defaultCoins,
		TradingCoins:         tradingCoins,
		SystemPromptTemplate: traderCfg.SystemPromptTemplate, // 系统提示词模板
	}

	// 根据交易所类型设置API密钥
	if exchangeCfg.ID == "binance" {
		traderConfig.BinanceAPIKey = exchangeCfg.APIKey
		traderConfig.BinanceSecretKey = exchangeCfg.SecretKey
	} else if exchangeCfg.ID == "hyperliquid" {
		traderConfig.HyperliquidPrivateKey = exchangeCfg.APIKey // hyperliquid用APIKey存储private key
		traderConfig.HyperliquidWalletAddr = exchangeCfg.HyperliquidWalletAddr
	} else if exchangeCfg.ID == "aster" {
		traderConfig.AsterUser = exchangeCfg.AsterUser
		traderConfig.AsterSigner = exchangeCfg.AsterSigner
		traderConfig.AsterPrivateKey = exchangeCfg.AsterPrivateKey
	}

	// 根据AI模型设置API密钥
	if aiModelCfg.Provider == "qwen" {
		traderConfig.QwenKey = aiModelCfg.APIKey
	} else if aiModelCfg.Provider == "deepseek" {
		traderConfig.DeepSeekKey = aiModelCfg.APIKey
	}

	// 创建trader实例
	at, err := trader.NewAutoTrader(traderConfig)
	if err != nil {
		return fmt.Errorf("创建trader失败: %w", err)
	}

	tm.attachDecisionReporter(at, traderCfg, supabaseClient)

	// 设置自定义prompt（如果有）
	if traderCfg.CustomPrompt != "" {
		at.SetCustomPrompt(traderCfg.CustomPrompt)
		at.SetOverrideBasePrompt(traderCfg.OverrideBasePrompt)
		if traderCfg.OverrideBasePrompt {
			log.Printf("✓ 已设置自定义交易策略prompt (覆盖基础prompt)")
		} else {
			log.Printf("✓ 已设置自定义交易策略prompt (补充基础prompt)")
		}
	}

	tm.traders[traderCfg.ID] = at
	log.Printf("✓ Trader '%s' (%s + %s) 已为用户加载到内存", traderCfg.Name, aiModelCfg.Provider, exchangeCfg.ID)
	return nil
}
