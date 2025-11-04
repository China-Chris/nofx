package config

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"log"
	"nofx/market"
	"slices"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Database 配置数据库
type Database struct {
	db *sql.DB
}

// NewDatabase 创建配置数据库
func NewDatabase(dbPath string) (*Database, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	database := &Database{db: db}
	if err := database.createTables(); err != nil {
		return nil, fmt.Errorf("创建表失败: %w", err)
	}

	if err := database.initDefaultData(); err != nil {
		return nil, fmt.Errorf("初始化默认数据失败: %w", err)
	}

	return database, nil
}

// createTables 创建数据库表
func (d *Database) createTables() error {
	queries := []string{
		// AI模型配置表
		`CREATE TABLE IF NOT EXISTS ai_models (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL DEFAULT 'default',
			name TEXT NOT NULL,
			provider TEXT NOT NULL,
			enabled BOOLEAN DEFAULT 0,
			api_key TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,

		// 交易所配置表
		`CREATE TABLE IF NOT EXISTS exchanges (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL DEFAULT 'default',
			name TEXT NOT NULL,
			type TEXT NOT NULL, -- 'cex' or 'dex'
			enabled BOOLEAN DEFAULT 0,
			api_key TEXT DEFAULT '',
			secret_key TEXT DEFAULT '',
			testnet BOOLEAN DEFAULT 0,
			-- Hyperliquid 特定字段
			hyperliquid_wallet_addr TEXT DEFAULT '',
			-- Aster 特定字段
			aster_user TEXT DEFAULT '',
			aster_signer TEXT DEFAULT '',
			aster_private_key TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,

		// 用户信号源配置表
		`CREATE TABLE IF NOT EXISTS user_signal_sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			coin_pool_url TEXT DEFAULT '',
			oi_top_url TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
			UNIQUE(user_id)
		)`,

		// 交易员配置表
		`CREATE TABLE IF NOT EXISTS traders (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL DEFAULT 'default',
			name TEXT NOT NULL,
			ai_model_id TEXT NOT NULL,
			exchange_id TEXT NOT NULL,
			initial_balance REAL NOT NULL,
			scan_interval_minutes INTEGER DEFAULT 3,
			is_running BOOLEAN DEFAULT 0,
			btc_eth_leverage INTEGER DEFAULT 5,
			altcoin_leverage INTEGER DEFAULT 5,
			trading_symbols TEXT DEFAULT '',
			use_coin_pool BOOLEAN DEFAULT 0,
			use_oi_top BOOLEAN DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (ai_model_id) REFERENCES ai_models(id),
			FOREIGN KEY (exchange_id) REFERENCES exchanges(id)
		)`,

		// 用户表
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			email TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			otp_secret TEXT,
			otp_verified BOOLEAN DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		// 系统配置表
		`CREATE TABLE IF NOT EXISTS system_config (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		// 触发器：自动更新 updated_at
		`CREATE TRIGGER IF NOT EXISTS update_users_updated_at
			AFTER UPDATE ON users
			BEGIN
				UPDATE users SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
			END`,

		`CREATE TRIGGER IF NOT EXISTS update_ai_models_updated_at
			AFTER UPDATE ON ai_models
			BEGIN
				UPDATE ai_models SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
			END`,

		`CREATE TRIGGER IF NOT EXISTS update_exchanges_updated_at
			AFTER UPDATE ON exchanges
			BEGIN
				UPDATE exchanges SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
			END`,

		`CREATE TRIGGER IF NOT EXISTS update_traders_updated_at
			AFTER UPDATE ON traders
			BEGIN
				UPDATE traders SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
			END`,

		`CREATE TRIGGER IF NOT EXISTS update_user_signal_sources_updated_at
			AFTER UPDATE ON user_signal_sources
			BEGIN
				UPDATE user_signal_sources SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
			END`,

		`CREATE TRIGGER IF NOT EXISTS update_system_config_updated_at
			AFTER UPDATE ON system_config
			BEGIN
				UPDATE system_config SET updated_at = CURRENT_TIMESTAMP WHERE key = NEW.key;
			END`,
	}

	for _, query := range queries {
		if _, err := d.db.Exec(query); err != nil {
			return fmt.Errorf("执行SQL失败 [%s]: %w", query, err)
		}
	}

	// 为现有数据库添加新字段（向后兼容）
	alterQueries := []string{
		`ALTER TABLE exchanges ADD COLUMN hyperliquid_wallet_addr TEXT DEFAULT ''`,
		`ALTER TABLE exchanges ADD COLUMN aster_user TEXT DEFAULT ''`,
		`ALTER TABLE exchanges ADD COLUMN aster_signer TEXT DEFAULT ''`,
		`ALTER TABLE exchanges ADD COLUMN aster_private_key TEXT DEFAULT ''`,
		`ALTER TABLE traders ADD COLUMN custom_prompt TEXT DEFAULT ''`,
		`ALTER TABLE traders ADD COLUMN override_base_prompt BOOLEAN DEFAULT 0`,
		`ALTER TABLE traders ADD COLUMN is_cross_margin BOOLEAN DEFAULT 1`,             // 默认为全仓模式
		`ALTER TABLE traders ADD COLUMN use_default_coins BOOLEAN DEFAULT 1`,           // 默认使用默认币种
		`ALTER TABLE traders ADD COLUMN custom_coins TEXT DEFAULT ''`,                  // 自定义币种列表（JSON格式）
		`ALTER TABLE traders ADD COLUMN btc_eth_leverage INTEGER DEFAULT 5`,            // BTC/ETH杠杆倍数
		`ALTER TABLE traders ADD COLUMN altcoin_leverage INTEGER DEFAULT 5`,            // 山寨币杠杆倍数
		`ALTER TABLE traders ADD COLUMN trading_symbols TEXT DEFAULT ''`,               // 交易币种，逗号分隔
		`ALTER TABLE traders ADD COLUMN use_coin_pool BOOLEAN DEFAULT 0`,               // 是否使用COIN POOL信号源
		`ALTER TABLE traders ADD COLUMN use_oi_top BOOLEAN DEFAULT 0`,                  // 是否使用OI TOP信号源
		`ALTER TABLE traders ADD COLUMN use_inside_coins BOOLEAN DEFAULT 0`,            // 是否使用内置AI评分信号源
		`ALTER TABLE traders ADD COLUMN system_prompt_template TEXT DEFAULT 'default'`, // 系统提示词模板名称
		`ALTER TABLE ai_models ADD COLUMN custom_api_url TEXT DEFAULT ''`,              // 自定义API地址
		`ALTER TABLE ai_models ADD COLUMN custom_model_name TEXT DEFAULT ''`,           // 自定义模型名称
	}

	for _, query := range alterQueries {
		// 忽略已存在字段的错误
		d.db.Exec(query)
	}

	// 检查是否需要迁移exchanges表的主键结构
	err := d.migrateExchangesTable()
	if err != nil {
		log.Printf("⚠️ 迁移exchanges表失败: %v", err)
	}

	return nil
}

// initDefaultData 初始化默认数据
func (d *Database) initDefaultData() error {
	// 初始化AI模型（使用default用户）
	aiModels := []struct {
		id, name, provider string
	}{
		{"deepseek", "DeepSeek", "deepseek"},
		{"qwen", "Qwen", "qwen"},
	}

	for _, model := range aiModels {
		_, err := d.db.Exec(`
			INSERT OR IGNORE INTO ai_models (id, user_id, name, provider, enabled) 
			VALUES (?, 'default', ?, ?, 0)
		`, model.id, model.name, model.provider)
		if err != nil {
			return fmt.Errorf("初始化AI模型失败: %w", err)
		}
	}

	// 初始化交易所（使用default用户）
	exchanges := []struct {
		id, name, typ string
	}{
		{"binance", "Binance Futures", "binance"},
		{"hyperliquid", "Hyperliquid", "hyperliquid"},
		{"aster", "Aster DEX", "aster"},
	}

	for _, exchange := range exchanges {
		_, err := d.db.Exec(`
			INSERT OR IGNORE INTO exchanges (id, user_id, name, type, enabled) 
			VALUES (?, 'default', ?, ?, 0)
		`, exchange.id, exchange.name, exchange.typ)
		if err != nil {
			return fmt.Errorf("初始化交易所失败: %w", err)
		}
	}

	// 初始化系统配置 - 创建所有字段，设置默认值，后续由config.json同步更新
	systemConfigs := map[string]string{
		"admin_mode":           "true",                                                                                // 默认开启管理员模式，便于首次使用
		"api_server_port":      "8080",                                                                                // 默认API端口
		"use_default_coins":    "true",                                                                                // 默认使用内置币种列表
		"default_coins":        `["BTCUSDT","ETHUSDT","SOLUSDT","BNBUSDT","XRPUSDT","DOGEUSDT","ADAUSDT","HYPEUSDT"]`, // 默认币种列表（JSON格式）
		"max_daily_loss":       "10.0",                                                                                // 最大日损失百分比
		"max_drawdown":         "20.0",                                                                                // 最大回撤百分比
		"stop_trading_minutes": "60",                                                                                  // 停止交易时间（分钟）
		"btc_eth_leverage":     "5",                                                                                   // BTC/ETH杠杆倍数
		"altcoin_leverage":     "5",                                                                                   // 山寨币杠杆倍数
		"jwt_secret":           "",                                                                                    // JWT密钥，默认为空，由config.json或系统生成
	}

	for key, value := range systemConfigs {
		_, err := d.db.Exec(`
			INSERT OR IGNORE INTO system_config (key, value) 
			VALUES (?, ?)
		`, key, value)
		if err != nil {
			return fmt.Errorf("初始化系统配置失败: %w", err)
		}
	}

	return nil
}

// migrateExchangesTable 迁移exchanges表支持多用户
func (d *Database) migrateExchangesTable() error {
	// 检查是否已经迁移过
	var count int
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master 
		WHERE type='table' AND name='exchanges_new'
	`).Scan(&count)
	if err != nil {
		return err
	}

	// 如果已经迁移过，直接返回
	if count > 0 {
		return nil
	}

	log.Printf("🔄 开始迁移exchanges表...")

	// 创建新的exchanges表，使用复合主键
	_, err = d.db.Exec(`
		CREATE TABLE exchanges_new (
			id TEXT NOT NULL,
			user_id TEXT NOT NULL DEFAULT 'default',
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			enabled BOOLEAN DEFAULT 0,
			api_key TEXT DEFAULT '',
			secret_key TEXT DEFAULT '',
			testnet BOOLEAN DEFAULT 0,
			hyperliquid_wallet_addr TEXT DEFAULT '',
			aster_user TEXT DEFAULT '',
			aster_signer TEXT DEFAULT '',
			aster_private_key TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (id, user_id),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return fmt.Errorf("创建新exchanges表失败: %w", err)
	}

	// 复制数据到新表
	_, err = d.db.Exec(`
		INSERT INTO exchanges_new 
		SELECT * FROM exchanges
	`)
	if err != nil {
		return fmt.Errorf("复制数据失败: %w", err)
	}

	// 删除旧表
	_, err = d.db.Exec(`DROP TABLE exchanges`)
	if err != nil {
		return fmt.Errorf("删除旧表失败: %w", err)
	}

	// 重命名新表
	_, err = d.db.Exec(`ALTER TABLE exchanges_new RENAME TO exchanges`)
	if err != nil {
		return fmt.Errorf("重命名表失败: %w", err)
	}

	// 重新创建触发器
	_, err = d.db.Exec(`
		CREATE TRIGGER IF NOT EXISTS update_exchanges_updated_at
			AFTER UPDATE ON exchanges
			BEGIN
				UPDATE exchanges SET updated_at = CURRENT_TIMESTAMP 
				WHERE id = NEW.id AND user_id = NEW.user_id;
			END
	`)
	if err != nil {
		return fmt.Errorf("创建触发器失败: %w", err)
	}

	log.Printf("✅ exchanges表迁移完成")
	return nil
}

// User 用户配置
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"` // 不返回到前端
	OTPSecret    string    `json:"-"` // 不返回到前端
	OTPVerified  bool      `json:"otp_verified"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// AIModelConfig AI模型配置
type AIModelConfig struct {
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	Name            string    `json:"name"`
	Provider        string    `json:"provider"`
	Enabled         bool      `json:"enabled"`
	APIKey          string    `json:"apiKey"`
	CustomAPIURL    string    `json:"customApiUrl"`
	CustomModelName string    `json:"customModelName"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ExchangeConfig 交易所配置
type ExchangeConfig struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Enabled   bool   `json:"enabled"`
	APIKey    string `json:"apiKey"`
	SecretKey string `json:"secretKey"`
	Testnet   bool   `json:"testnet"`
	// Hyperliquid 特定字段
	HyperliquidWalletAddr string `json:"hyperliquidWalletAddr"`
	// Aster 特定字段
	AsterUser       string    `json:"asterUser"`
	AsterSigner     string    `json:"asterSigner"`
	AsterPrivateKey string    `json:"asterPrivateKey"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// TraderRecord 交易员配置（数据库实体）
type TraderRecord struct {
	ID                   string    `json:"id"`
	UserID               string    `json:"user_id"`
	Name                 string    `json:"name"`
	AIModelID            string    `json:"ai_model_id"`
	ExchangeID           string    `json:"exchange_id"`
	InitialBalance       float64   `json:"initial_balance"`
	ScanIntervalMinutes  int       `json:"scan_interval_minutes"`
	IsRunning            bool      `json:"is_running"`
	BTCETHLeverage       int       `json:"btc_eth_leverage"`       // BTC/ETH杠杆倍数
	AltcoinLeverage      int       `json:"altcoin_leverage"`       // 山寨币杠杆倍数
	TradingSymbols       string    `json:"trading_symbols"`        // 交易币种，逗号分隔
	UseCoinPool          bool      `json:"use_coin_pool"`          // 是否使用COIN POOL信号源
	UseOITop             bool      `json:"use_oi_top"`             // 是否使用OI TOP信号源
	UseInsideCoins       bool      `json:"use_inside_coins"`       // 是否使用内置评分信号源
	CustomPrompt         string    `json:"custom_prompt"`          // 自定义交易策略prompt
	OverrideBasePrompt   bool      `json:"override_base_prompt"`   // 是否覆盖基础prompt
	SystemPromptTemplate string    `json:"system_prompt_template"` // 系统提示词模板名称
	IsCrossMargin        bool      `json:"is_cross_margin"`        // 是否为全仓模式（true=全仓，false=逐仓）
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// UserSignalSource 用户信号源配置
type UserSignalSource struct {
	ID          int       `json:"id"`
	UserID      string    `json:"user_id"`
	CoinPoolURL string    `json:"coin_pool_url"`
	OITopURL    string    `json:"oi_top_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// GenerateOTPSecret 生成OTP密钥
func GenerateOTPSecret() (string, error) {
	secret := make([]byte, 20)
	_, err := rand.Read(secret)
	if err != nil {
		return "", err
	}
	return base32.StdEncoding.EncodeToString(secret), nil
}

// CreateUser 创建用户
func (d *Database) CreateUser(user *User) error {
	_, err := d.db.Exec(`
		INSERT INTO users (id, email, password_hash, otp_secret, otp_verified)
		VALUES (?, ?, ?, ?, ?)
	`, user.ID, user.Email, user.PasswordHash, user.OTPSecret, user.OTPVerified)
	return err
}

// EnsureAdminUser 确保admin用户存在（用于管理员模式）
func (d *Database) EnsureAdminUser() error {
	// 检查admin用户是否已存在
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = 'admin'`).Scan(&count)
	if err != nil {
		return err
	}

	// 如果已存在，直接返回
	if count > 0 {
		return nil
	}

	// 创建admin用户（密码为空，因为管理员模式下不需要密码）
	adminUser := &User{
		ID:           "admin",
		Email:        "admin@localhost",
		PasswordHash: "", // 管理员模式下不使用密码
		OTPSecret:    "",
		OTPVerified:  true,
	}

	return d.CreateUser(adminUser)
}

// GetUserByEmail 通过邮箱获取用户
func (d *Database) GetUserByEmail(email string) (*User, error) {
	var user User
	err := d.db.QueryRow(`
		SELECT id, email, password_hash, otp_secret, otp_verified, created_at, updated_at
		FROM users WHERE email = ?
	`, email).Scan(
		&user.ID, &user.Email, &user.PasswordHash, &user.OTPSecret,
		&user.OTPVerified, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// GetUserByID 通过ID获取用户
func (d *Database) GetUserByID(userID string) (*User, error) {
	var user User
	err := d.db.QueryRow(`
		SELECT id, email, password_hash, otp_secret, otp_verified, created_at, updated_at
		FROM users WHERE id = ?
	`, userID).Scan(
		&user.ID, &user.Email, &user.PasswordHash, &user.OTPSecret,
		&user.OTPVerified, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// GetAllUsers 获取所有用户ID列表
func (d *Database) GetAllUsers() ([]string, error) {
	rows, err := d.db.Query(`SELECT id FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var userIDs []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		userIDs = append(userIDs, userID)
	}
	return userIDs, nil
}

// UpdateUserOTPVerified 更新用户OTP验证状态
func (d *Database) UpdateUserOTPVerified(userID string, verified bool) error {
	_, err := d.db.Exec(`UPDATE users SET otp_verified = ? WHERE id = ?`, verified, userID)
	return err
}

// GetAIModels 获取用户的AI模型配置
func (d *Database) GetAIModels(userID string) ([]*AIModelConfig, error) {
	rows, err := d.db.Query(`
		SELECT id, user_id, name, provider, enabled, api_key,
		       COALESCE(custom_api_url, '') as custom_api_url,
		       COALESCE(custom_model_name, '') as custom_model_name,
		       created_at, updated_at
		FROM ai_models WHERE user_id = ? ORDER BY id
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 初始化为空切片而不是nil，确保JSON序列化为[]而不是null
	models := make([]*AIModelConfig, 0)
	for rows.Next() {
		var model AIModelConfig
		err := rows.Scan(
			&model.ID, &model.UserID, &model.Name, &model.Provider,
			&model.Enabled, &model.APIKey, &model.CustomAPIURL, &model.CustomModelName,
			&model.CreatedAt, &model.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		models = append(models, &model)
	}

	return models, nil
}

// UpdateAIModel 更新AI模型配置，如果不存在则创建用户特定配置
func (d *Database) UpdateAIModel(userID, id string, enabled bool, apiKey, customAPIURL, customModelName string) error {
	// 先尝试精确匹配 ID（新版逻辑，支持多个相同 provider 的模型）
	var existingID string
	err := d.db.QueryRow(`
		SELECT id FROM ai_models WHERE user_id = ? AND id = ? LIMIT 1
	`, userID, id).Scan(&existingID)

	if err == nil {
		// 找到了现有配置（精确匹配 ID），更新它
		_, err = d.db.Exec(`
			UPDATE ai_models SET enabled = ?, api_key = ?, custom_api_url = ?, custom_model_name = ?, updated_at = datetime('now')
			WHERE id = ? AND user_id = ?
		`, enabled, apiKey, customAPIURL, customModelName, existingID, userID)
		return err
	}

	// ID 不存在，尝试兼容旧逻辑：将 id 作为 provider 查找
	provider := id
	err = d.db.QueryRow(`
		SELECT id FROM ai_models WHERE user_id = ? AND provider = ? LIMIT 1
	`, userID, provider).Scan(&existingID)

	if err == nil {
		// 找到了现有配置（通过 provider 匹配，兼容旧版），更新它
		log.Printf("⚠️  使用旧版 provider 匹配更新模型: %s -> %s", provider, existingID)
		_, err = d.db.Exec(`
			UPDATE ai_models SET enabled = ?, api_key = ?, custom_api_url = ?, custom_model_name = ?, updated_at = datetime('now')
			WHERE id = ? AND user_id = ?
		`, enabled, apiKey, customAPIURL, customModelName, existingID, userID)
		return err
	}

	// 没有找到任何现有配置，创建新的
	// 推断 provider（从 id 中提取，或者直接使用 id）
	if provider == id && (provider == "deepseek" || provider == "qwen") {
		// id 本身就是 provider
		provider = id
	} else {
		// 从 id 中提取 provider（假设格式是 userID_provider 或 timestamp_userID_provider）
		parts := strings.Split(id, "_")
		if len(parts) >= 2 {
			provider = parts[len(parts)-1] // 取最后一部分作为 provider
		} else {
			provider = id
		}
	}

	// 获取模型的基本信息
	var name string
	err = d.db.QueryRow(`
		SELECT name FROM ai_models WHERE provider = ? LIMIT 1
	`, provider).Scan(&name)
	if err != nil {
		// 如果找不到基本信息，使用默认值
		if provider == "deepseek" {
			name = "DeepSeek AI"
		} else if provider == "qwen" {
			name = "Qwen AI"
		} else {
			name = provider + " AI"
		}
	}

	// 如果传入的 ID 已经是完整格式（如 "admin_deepseek_custom1"），直接使用
	// 否则生成新的 ID
	newModelID := id
	if id == provider {
		// id 就是 provider，生成新的用户特定 ID
		newModelID = fmt.Sprintf("%s_%s", userID, provider)
	}

	log.Printf("✓ 创建新的 AI 模型配置: ID=%s, Provider=%s, Name=%s", newModelID, provider, name)
	_, err = d.db.Exec(`
		INSERT INTO ai_models (id, user_id, name, provider, enabled, api_key, custom_api_url, custom_model_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	`, newModelID, userID, name, provider, enabled, apiKey, customAPIURL, customModelName)

	return err
}

// GetExchanges 获取用户的交易所配置
func (d *Database) GetExchanges(userID string) ([]*ExchangeConfig, error) {
	rows, err := d.db.Query(`
		SELECT id, user_id, name, type, enabled, api_key, secret_key, testnet, 
		       COALESCE(hyperliquid_wallet_addr, '') as hyperliquid_wallet_addr,
		       COALESCE(aster_user, '') as aster_user,
		       COALESCE(aster_signer, '') as aster_signer,
		       COALESCE(aster_private_key, '') as aster_private_key,
		       created_at, updated_at 
		FROM exchanges WHERE user_id = ? ORDER BY id
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 初始化为空切片而不是nil，确保JSON序列化为[]而不是null
	exchanges := make([]*ExchangeConfig, 0)
	for rows.Next() {
		var exchange ExchangeConfig
		err := rows.Scan(
			&exchange.ID, &exchange.UserID, &exchange.Name, &exchange.Type,
			&exchange.Enabled, &exchange.APIKey, &exchange.SecretKey, &exchange.Testnet,
			&exchange.HyperliquidWalletAddr, &exchange.AsterUser,
			&exchange.AsterSigner, &exchange.AsterPrivateKey,
			&exchange.CreatedAt, &exchange.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		exchanges = append(exchanges, &exchange)
	}

	return exchanges, nil
}

// UpdateExchange 更新交易所配置，如果不存在则创建用户特定配置
func (d *Database) UpdateExchange(userID, id string, enabled bool, apiKey, secretKey string, testnet bool, hyperliquidWalletAddr, asterUser, asterSigner, asterPrivateKey string) error {
	log.Printf("🔧 UpdateExchange: userID=%s, id=%s, enabled=%v", userID, id, enabled)

	// 首先尝试更新现有的用户配置
	result, err := d.db.Exec(`
		UPDATE exchanges SET enabled = ?, api_key = ?, secret_key = ?, testnet = ?, 
		       hyperliquid_wallet_addr = ?, aster_user = ?, aster_signer = ?, aster_private_key = ?, updated_at = datetime('now')
		WHERE id = ? AND user_id = ?
	`, enabled, apiKey, secretKey, testnet, hyperliquidWalletAddr, asterUser, asterSigner, asterPrivateKey, id, userID)
	if err != nil {
		log.Printf("❌ UpdateExchange: 更新失败: %v", err)
		return err
	}

	// 检查是否有行被更新
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("❌ UpdateExchange: 获取影响行数失败: %v", err)
		return err
	}

	log.Printf("📊 UpdateExchange: 影响行数 = %d", rowsAffected)

	// 如果没有行被更新，说明用户没有这个交易所的配置，需要创建
	if rowsAffected == 0 {
		log.Printf("💡 UpdateExchange: 没有现有记录，创建新记录")

		// 根据交易所ID确定基本信息
		var name, typ string
		if id == "binance" {
			name = "Binance Futures"
			typ = "cex"
		} else if id == "hyperliquid" {
			name = "Hyperliquid"
			typ = "dex"
		} else if id == "aster" {
			name = "Aster DEX"
			typ = "dex"
		} else {
			name = id + " Exchange"
			typ = "cex"
		}

		log.Printf("🆕 UpdateExchange: 创建新记录 ID=%s, name=%s, type=%s", id, name, typ)

		// 创建用户特定的配置，使用原始的交易所ID
		_, err = d.db.Exec(`
			INSERT INTO exchanges (id, user_id, name, type, enabled, api_key, secret_key, testnet, 
			                       hyperliquid_wallet_addr, aster_user, aster_signer, aster_private_key, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
		`, id, userID, name, typ, enabled, apiKey, secretKey, testnet, hyperliquidWalletAddr, asterUser, asterSigner, asterPrivateKey)

		if err != nil {
			log.Printf("❌ UpdateExchange: 创建记录失败: %v", err)
		} else {
			log.Printf("✅ UpdateExchange: 创建记录成功")
		}
		return err
	}

	log.Printf("✅ UpdateExchange: 更新现有记录成功")
	return nil
}

// CreateAIModel 创建AI模型配置
func (d *Database) CreateAIModel(userID, id, name, provider string, enabled bool, apiKey, customAPIURL string) error {
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO ai_models (id, user_id, name, provider, enabled, api_key, custom_api_url) 
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, userID, name, provider, enabled, apiKey, customAPIURL)
	return err
}

// CreateExchange 创建交易所配置
func (d *Database) CreateExchange(userID, id, name, typ string, enabled bool, apiKey, secretKey string, testnet bool, hyperliquidWalletAddr, asterUser, asterSigner, asterPrivateKey string) error {
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO exchanges (id, user_id, name, type, enabled, api_key, secret_key, testnet, hyperliquid_wallet_addr, aster_user, aster_signer, aster_private_key) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, userID, name, typ, enabled, apiKey, secretKey, testnet, hyperliquidWalletAddr, asterUser, asterSigner, asterPrivateKey)
	return err
}

// CreateTrader 创建交易员
func (d *Database) CreateTrader(trader *TraderRecord) error {
	_, err := d.db.Exec(`
		INSERT INTO traders (id, user_id, name, ai_model_id, exchange_id, initial_balance, scan_interval_minutes, is_running, btc_eth_leverage, altcoin_leverage, trading_symbols, use_coin_pool, use_oi_top, use_inside_coins, custom_prompt, override_base_prompt, system_prompt_template, is_cross_margin)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,?, ?, ?, ?, ?, ?)
	`, trader.ID, trader.UserID, trader.Name, trader.AIModelID, trader.ExchangeID, trader.InitialBalance, trader.ScanIntervalMinutes, trader.IsRunning, trader.BTCETHLeverage, trader.AltcoinLeverage, trader.TradingSymbols, trader.UseCoinPool, trader.UseOITop, trader.UseInsideCoins, trader.CustomPrompt, trader.OverrideBasePrompt, trader.SystemPromptTemplate, trader.IsCrossMargin)
	return err
}

// GetTraders 获取用户的交易员
func (d *Database) GetTraders(userID string) ([]*TraderRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, user_id, name, ai_model_id, exchange_id, initial_balance, scan_interval_minutes, is_running,
		       COALESCE(btc_eth_leverage, 5) as btc_eth_leverage, COALESCE(altcoin_leverage, 5) as altcoin_leverage,
		       COALESCE(trading_symbols, '') as trading_symbols,
		       COALESCE(use_coin_pool, 0) as use_coin_pool, COALESCE(use_oi_top, 0) as use_oi_top,COALESCE(use_inside_coins, 0) as use_inside_coins,
		       COALESCE(custom_prompt, '') as custom_prompt, COALESCE(override_base_prompt, 0) as override_base_prompt,
		       COALESCE(system_prompt_template, 'default') as system_prompt_template,
		       COALESCE(is_cross_margin, 1) as is_cross_margin, created_at, updated_at
		FROM traders WHERE user_id = ? ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var traders []*TraderRecord
	for rows.Next() {
		var trader TraderRecord
		err := rows.Scan(
			&trader.ID, &trader.UserID, &trader.Name, &trader.AIModelID, &trader.ExchangeID,
			&trader.InitialBalance, &trader.ScanIntervalMinutes, &trader.IsRunning,
			&trader.BTCETHLeverage, &trader.AltcoinLeverage, &trader.TradingSymbols,
			&trader.UseCoinPool, &trader.UseOITop, &trader.UseInsideCoins,
			&trader.CustomPrompt, &trader.OverrideBasePrompt, &trader.SystemPromptTemplate,
			&trader.IsCrossMargin,
			&trader.CreatedAt, &trader.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		traders = append(traders, &trader)
	}

	return traders, nil
}

// UpdateTraderStatus 更新交易员状态
func (d *Database) UpdateTraderStatus(userID, id string, isRunning bool) error {
	_, err := d.db.Exec(`UPDATE traders SET is_running = ? WHERE id = ? AND user_id = ?`, isRunning, id, userID)
	return err
}

// UpdateTrader 更新交易员配置
func (d *Database) UpdateTrader(trader *TraderRecord) error {
	_, err := d.db.Exec(`
		UPDATE traders SET
			name = ?, ai_model_id = ?, exchange_id = ?, initial_balance = ?,
			scan_interval_minutes = ?, btc_eth_leverage = ?, altcoin_leverage = ?,
			trading_symbols = ?, custom_prompt = ?, override_base_prompt = ?,
			system_prompt_template = ?, is_cross_margin = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ?
	`, trader.Name, trader.AIModelID, trader.ExchangeID, trader.InitialBalance,
		trader.ScanIntervalMinutes, trader.BTCETHLeverage, trader.AltcoinLeverage,
		trader.TradingSymbols, trader.CustomPrompt, trader.OverrideBasePrompt,
		trader.SystemPromptTemplate, trader.IsCrossMargin, trader.ID, trader.UserID)
	return err
}

// UpdateTraderCustomPrompt 更新交易员自定义Prompt
func (d *Database) UpdateTraderCustomPrompt(userID, id string, customPrompt string, overrideBase bool) error {
	_, err := d.db.Exec(`UPDATE traders SET custom_prompt = ?, override_base_prompt = ? WHERE id = ? AND user_id = ?`, customPrompt, overrideBase, id, userID)
	return err
}

// DeleteTrader 删除交易员
func (d *Database) DeleteTrader(userID, id string) error {
	_, err := d.db.Exec(`DELETE FROM traders WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// GetTraderConfig 获取交易员完整配置（包含AI模型和交易所信息）
func (d *Database) GetTraderConfig(userID, traderID string) (*TraderRecord, *AIModelConfig, *ExchangeConfig, error) {
	trader, err := d.fetchTraderRecord("id = ? AND user_id = ?", traderID, userID)
	if err != nil {
		if err != sql.ErrNoRows {
			return nil, nil, nil, err
		}

		var candidates []string
		if extracted := extractTGIDFromSlug(traderID); extracted != "" {
			candidates = append(candidates, extracted)
		}
		if userID != "" && userID != "admin" {
			candidates = append(candidates, userID)
		}

		seen := make(map[string]struct{})
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}

			trader, err = d.fetchTraderRecord("name = ? AND user_id = ?", traderID, candidate)
			if err == nil {
				break
			}
			if err != sql.ErrNoRows {
				return nil, nil, nil, err
			}
		}

		if err != nil {
			return nil, nil, nil, err
		}
	}

	ownerUserID := trader.UserID

	aiModel, err := d.lookupAIModel(ownerUserID, trader.AIModelID)
	if err != nil && err != sql.ErrNoRows {
		return nil, nil, nil, err
	}
	if aiModel == nil && ownerUserID != "default" {
		if fallback, ferr := d.lookupAIModel("default", trader.AIModelID); ferr == nil || ferr == sql.ErrNoRows {
			aiModel = fallback
		} else {
			return nil, nil, nil, ferr
		}
	}

	exchange, err := d.lookupExchange(ownerUserID, trader.ExchangeID)
	if err != nil && err != sql.ErrNoRows {
		return nil, nil, nil, err
	}
	if exchange == nil && ownerUserID != "default" {
		if fallback, ferr := d.lookupExchange("default", trader.ExchangeID); ferr == nil || ferr == sql.ErrNoRows {
			exchange = fallback
		} else {
			return nil, nil, nil, ferr
		}
	}

	return trader, aiModel, exchange, nil
}

// lookupAIModel 返回指定用户的AI模型（若不存在返回sql.ErrNoRows）
func (d *Database) lookupAIModel(userID, modelID string) (*AIModelConfig, error) {
	if strings.TrimSpace(modelID) == "" {
		return nil, sql.ErrNoRows
	}

	var model AIModelConfig
	err := d.db.QueryRow(`
		SELECT id, user_id, name, provider, enabled, api_key,
		       COALESCE(custom_api_url, '')   AS custom_api_url,
		       COALESCE(custom_model_name, '') AS custom_model_name,
		       created_at, updated_at
		FROM ai_models
		WHERE user_id = ? AND (id = ? OR provider = ?)
		LIMIT 1
	`, userID, modelID, modelID).Scan(
		&model.ID, &model.UserID, &model.Name, &model.Provider, &model.Enabled, &model.APIKey,
		&model.CustomAPIURL, &model.CustomModelName, &model.CreatedAt, &model.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &model, nil
}

// lookupExchange 返回指定用户的交易所配置（若不存在返回sql.ErrNoRows）
func (d *Database) lookupExchange(userID, exchangeID string) (*ExchangeConfig, error) {
	if strings.TrimSpace(exchangeID) == "" {
		return nil, sql.ErrNoRows
	}

	var exchange ExchangeConfig
	err := d.db.QueryRow(`
		SELECT id, user_id, name, type, enabled, api_key, secret_key, testnet,
		       COALESCE(hyperliquid_wallet_addr, '') AS hyperliquid_wallet_addr,
		       COALESCE(aster_user, '')             AS aster_user,
		       COALESCE(aster_signer, '')           AS aster_signer,
		       COALESCE(aster_private_key, '')      AS aster_private_key,
		       created_at, updated_at
		FROM exchanges
		WHERE user_id = ? AND id = ?
		LIMIT 1
	`, userID, exchangeID).Scan(
		&exchange.ID, &exchange.UserID, &exchange.Name, &exchange.Type, &exchange.Enabled,
		&exchange.APIKey, &exchange.SecretKey, &exchange.Testnet,
		&exchange.HyperliquidWalletAddr, &exchange.AsterUser, &exchange.AsterSigner, &exchange.AsterPrivateKey,
		&exchange.CreatedAt, &exchange.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &exchange, nil
}

func (d *Database) fetchTraderRecord(where string, args ...interface{}) (*TraderRecord, error) {
	query := `
		SELECT 
			id, user_id, name, ai_model_id, exchange_id,
			initial_balance, scan_interval_minutes, is_running,
			COALESCE(btc_eth_leverage, 5)     AS btc_eth_leverage,
			COALESCE(altcoin_leverage, 5)     AS altcoin_leverage,
			COALESCE(trading_symbols, '')     AS trading_symbols,
			COALESCE(use_coin_pool, 0)        AS use_coin_pool,
			COALESCE(use_oi_top, 0)           AS use_oi_top,
			COALESCE(use_inside_coins, 0)     AS use_inside_coins,
			COALESCE(custom_prompt, '')       AS custom_prompt,
			COALESCE(override_base_prompt, 0) AS override_base_prompt,
			COALESCE(system_prompt_template, 'default') AS system_prompt_template,
			COALESCE(is_cross_margin, 1)      AS is_cross_margin,
			created_at, updated_at
		FROM traders
		WHERE ` + where + `
		LIMIT 1
	`

	var trader TraderRecord
	err := d.db.QueryRow(query, args...).Scan(
		&trader.ID, &trader.UserID, &trader.Name, &trader.AIModelID, &trader.ExchangeID,
		&trader.InitialBalance, &trader.ScanIntervalMinutes, &trader.IsRunning,
		&trader.BTCETHLeverage, &trader.AltcoinLeverage, &trader.TradingSymbols,
		&trader.UseCoinPool, &trader.UseOITop, &trader.UseInsideCoins,
		&trader.CustomPrompt, &trader.OverrideBasePrompt, &trader.SystemPromptTemplate,
		&trader.IsCrossMargin, &trader.CreatedAt, &trader.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &trader, nil
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

// GetSystemConfig 获取系统配置
func (d *Database) GetSystemConfig(key string) (string, error) {
	var value string
	err := d.db.QueryRow(`SELECT value FROM system_config WHERE key = ?`, key).Scan(&value)
	return value, err
}

// SetSystemConfig 设置系统配置
func (d *Database) SetSystemConfig(key, value string) error {
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO system_config (key, value) VALUES (?, ?)
	`, key, value)
	return err
}

// CreateUserSignalSource 创建用户信号源配置
func (d *Database) CreateUserSignalSource(userID, coinPoolURL, oiTopURL string) error {
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO user_signal_sources (user_id, coin_pool_url, oi_top_url, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
	`, userID, coinPoolURL, oiTopURL)
	return err
}

// GetUserSignalSource 获取用户信号源配置
func (d *Database) GetUserSignalSource(userID string) (*UserSignalSource, error) {
	var source UserSignalSource
	err := d.db.QueryRow(`
		SELECT id, user_id, coin_pool_url, oi_top_url, created_at, updated_at
		FROM user_signal_sources WHERE user_id = ?
	`, userID).Scan(
		&source.ID, &source.UserID, &source.CoinPoolURL, &source.OITopURL,
		&source.CreatedAt, &source.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &source, nil
}

// UpdateUserSignalSource 更新用户信号源配置
func (d *Database) UpdateUserSignalSource(userID, coinPoolURL, oiTopURL string) error {
	_, err := d.db.Exec(`
		UPDATE user_signal_sources SET coin_pool_url = ?, oi_top_url = ?, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ?
	`, coinPoolURL, oiTopURL, userID)
	return err
}

// GetCustomCoins 获取所有交易员自定义币种 / Get all trader-customized currencies
func (d *Database) GetCustomCoins() []string {
	var symbol string
	var symbols []string
	_ = d.db.QueryRow(`
		SELECT GROUP_CONCAT(custom_coins , ',') as symbol
		FROM main.traders where custom_coins != ''
	`).Scan(&symbol)
	// 检测用户是否未配置币种 - 兼容性
	if symbol == "" {
		symbolJSON, _ := d.GetSystemConfig("default_coins")
		if err := json.Unmarshal([]byte(symbolJSON), &symbols); err != nil {
			log.Printf("⚠️  解析default_coins配置失败: %v，使用硬编码默认值", err)
			symbols = []string{"BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT"}
		}
	}
	// filter Symbol
	for _, s := range strings.Split(symbol, ",") {
		if s == "" {
			continue
		}
		coin := market.Normalize(s)
		if !slices.Contains(symbols, coin) {
			symbols = append(symbols, coin)
		}
	}
	return symbols
}

// Close 关闭数据库连接
func (d *Database) Close() error {
	return d.db.Close()
}
