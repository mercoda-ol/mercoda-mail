package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Email 是入库的邮件记录
type Email struct {
	ID        int64     `json:"id"`
	Account   string    `json:"account"`
	UID       uint32    `json:"uid"`
	From      string    `json:"from"`
	Subject   string    `json:"subject"`
	Category  string    `json:"category"`
	RuleName  string    `json:"rule_name"`
	Body      string    `json:"body"`
	HTMLBody  string    `json:"html_body"`
	Folder    string    `json:"folder"`
	IsRead    bool      `json:"is_read"`
	Timestamp time.Time `json:"timestamp"`
}

// Store 封装 SQLite 操作
type Store struct {
	db *sql.DB
}

// New 打开数据库并初始化表结构
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 单连接：WAL 下串行访问即可，防 SQLITE_BUSY 并减少连接内存
	db.SetMaxOpenConns(1)

	// WAL 模式，提升并发读写性能
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("设置 WAL 模式失败: %w", err)
	}

	store := &Store{db: db}
	if err := store.init(); err != nil {
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}

	return store, nil
}

func (s *Store) init() error {
	schema := `
	CREATE TABLE IF NOT EXISTS emails (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account TEXT NOT NULL,
		uid INTEGER NOT NULL,
		folder TEXT NOT NULL DEFAULT 'INBOX',
		"from" TEXT,
		subject TEXT,
		category TEXT,
		rule_name TEXT,
		body TEXT,
		html_body TEXT,
		is_read BOOLEAN NOT NULL DEFAULT 0,
		timestamp DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(account, folder, uid)
	);

	CREATE TABLE IF NOT EXISTS sync_state (
		account TEXT NOT NULL,
		folder TEXT NOT NULL DEFAULT 'INBOX',
		last_uid INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (account, folder)
	);

	CREATE TABLE IF NOT EXISTS rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		category TEXT NOT NULL,
		alert BOOLEAN NOT NULL DEFAULT 1,
		keywords TEXT NOT NULL, -- JSON 数组
		priority INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		email TEXT NOT NULL,
		provider TEXT NOT NULL,
		imap_host TEXT NOT NULL,
		imap_port INTEGER NOT NULL DEFAULT 993,
		auth_type TEXT NOT NULL, -- app_password / oauth2
		auth_username TEXT NOT NULL,
		auth_password_enc TEXT NOT NULL DEFAULT '' -- AES 加密（app_password 模式）
	);

	DROP TABLE IF EXISTS alerts; -- 告警模块已简化：alert 仅作通知开关，不再入库
	`
	_, err := s.db.Exec(schema)
	if err != nil {
		return err
	}
	return s.migrate()
}

// migrate 旧库结构迁移（emails.is_read / html_body 等新增列）
func (s *Store) migrate() error {
	rows, err := s.db.Query(`PRAGMA table_info(emails)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	hasRead, hasHTML := false, false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "is_read" {
			hasRead = true
		}
		if name == "html_body" {
			hasHTML = true
		}
	}
	if !hasRead {
		if _, err := s.db.Exec(`ALTER TABLE emails ADD COLUMN is_read BOOLEAN NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("迁移 emails.is_read 失败: %w", err)
		}
	}
	if !hasHTML {
		if _, err := s.db.Exec(`ALTER TABLE emails ADD COLUMN html_body TEXT`); err != nil {
			return fmt.Errorf("迁移 emails.html_body 失败: %w", err)
		}
	}
	return nil
}

// SaveEmail 保存邮件，同一 (account, folder, uid) 不重复入库；返回 email ID 和是否新插入
func (s *Store) SaveEmail(e *Email) (int64, bool, error) {
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO emails (account, uid, folder, "from", subject, category, rule_name, body, html_body, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, e.Account, e.UID, e.Folder, e.From, e.Subject, e.Category, e.RuleName, e.Body, e.HTMLBody, e.Timestamp)
	if err != nil {
		return 0, false, fmt.Errorf("保存邮件失败: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}

	if affected == 0 {
		// 已存在，查出 ID
		var id int64
		err := s.db.QueryRow(`SELECT id FROM emails WHERE account=? AND folder=? AND uid=?`, e.Account, e.Folder, e.UID).Scan(&id)
		return id, false, err
	}

	id, err := res.LastInsertId()
	return id, true, err
}

// GetLastUID 获取账号在指定 folder 的最大已同步 UID
func (s *Store) GetLastUID(account, folder string) (uint32, error) {
	var uid uint32
	err := s.db.QueryRow(`
		SELECT last_uid FROM sync_state WHERE account=? AND folder=?
	`, account, folder).Scan(&uid)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return uid, err
}

// UpdateLastUID 更新账号在指定 folder 的最大已同步 UID
func (s *Store) UpdateLastUID(account, folder string, uid uint32) error {
	_, err := s.db.Exec(`
		INSERT INTO sync_state (account, folder, last_uid, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(account, folder) DO UPDATE SET last_uid=excluded.last_uid, updated_at=CURRENT_TIMESTAMP
	`, account, folder, uid)
	return err
}

// Close 关闭数据库
func (s *Store) Close() error {
	return s.db.Close()
}

// EmailFilter 邮件查询过滤条件
type EmailFilter struct {
	Account  string
	Category string
	Query    string // 主题模糊搜索
	Page     int    // 从 1 开始
	PageSize int
}

// ListEmails 分页查询邮件（可按账号/分类/主题过滤）
func (s *Store) ListEmails(f EmailFilter) ([]Email, int, error) {
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}

	where := []string{"1=1"}
	args := []interface{}{}
	if f.Account != "" {
		where = append(where, "account=?")
		args = append(args, f.Account)
	}
	if f.Category != "" {
		where = append(where, "category=?")
		args = append(args, f.Category)
	}
	if f.Query != "" {
		where = append(where, "subject LIKE ?")
		args = append(args, "%"+f.Query+"%")
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM emails WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	offset := (f.Page - 1) * f.PageSize
	q := `SELECT id, account, uid, "from", subject, category, rule_name, is_read, timestamp
	      FROM emails WHERE ` + whereSQL + ` ORDER BY timestamp DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(q, append(args, f.PageSize, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	list := make([]Email, 0, f.PageSize)
	for rows.Next() {
		var e Email
		if err := rows.Scan(&e.ID, &e.Account, &e.UID, &e.From, &e.Subject, &e.Category, &e.RuleName, &e.IsRead, &e.Timestamp); err != nil {
			return nil, 0, err
		}
		list = append(list, e)
	}
	return list, total, rows.Err()
}

// UnreadCounts 各分类未读数（全账号汇总）
func (s *Store) UnreadCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT category, COUNT(*) FROM emails WHERE is_read=0 GROUP BY category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var cat string
		var n int
		if err := rows.Scan(&cat, &n); err != nil {
			return nil, err
		}
		out[cat] = n
	}
	return out, rows.Err()
}

// GetEmail 按 ID 查询单封邮件（含正文），不存在返回 nil
func (s *Store) GetEmail(id int64) (*Email, error) {
	var e Email
	err := s.db.QueryRow(`
		SELECT id, account, uid, folder, "from", subject, category, rule_name, body, html_body, is_read, timestamp
		FROM emails WHERE id=?
	`, id).Scan(&e.ID, &e.Account, &e.UID, &e.Folder, &e.From, &e.Subject, &e.Category, &e.RuleName, &e.Body, &e.HTMLBody, &e.IsRead, &e.Timestamp)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// MarkRead 标记单封已读
func (s *Store) MarkRead(id int64) error {
	_, err := s.db.Exec(`UPDATE emails SET is_read=1 WHERE id=?`, id)
	return err
}

// MarkReadAll 批量标记已读（category 为空则全部）
func (s *Store) MarkReadAll(category string) error {
	if category == "" {
		_, err := s.db.Exec(`UPDATE emails SET is_read=1 WHERE is_read=0`)
		return err
	}
	_, err := s.db.Exec(`UPDATE emails SET is_read=1 WHERE is_read=0 AND category=?`, category)
	return err
}

// ReclassifyAll 用当前规则重新分类全部已有邮件，返回变更条数
// classifyFn: (subject) -> (category, ruleName)；规则保存/删除后调用
// 单事务流式处理：边读边更新，不把全量 id/subject 载入内存，且只提交一次
func (s *Store) ReclassifyAll(classifyFn func(subject string) (category, ruleName string)) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id, subject FROM emails`)
	if err != nil {
		return 0, err
	}

	var changed int64
	for rows.Next() {
		var id int64
		var subject string
		if err := rows.Scan(&id, &subject); err != nil {
			rows.Close()
			return changed, err
		}
		cat, ruleName := classifyFn(subject)
		res, err := tx.Exec(`UPDATE emails SET category=?, rule_name=? WHERE id=? AND (category IS NULL OR category!=? OR IFNULL(rule_name,'')!=?)`,
			cat, ruleName, id, cat, ruleName)
		if err != nil {
			rows.Close()
			return changed, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return changed, err
	}
	if err := tx.Commit(); err != nil {
		return changed, err
	}
	return changed, nil
}

// DeleteEmail 删除单封邮件
func (s *Store) DeleteEmail(id int64) error {
	_, err := s.db.Exec(`DELETE FROM emails WHERE id=?`, id)
	return err
}

// DeleteEmails 批量删除邮件（单事务）
func (s *Store) DeleteEmails(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`DELETE FROM emails WHERE id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Rule 分类规则（持久化到 DB，支持 Web 在线编辑）
type Rule struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Alert    bool     `json:"alert"`
	Keywords []string `json:"keywords"`
	Priority int      `json:"priority"`
}

// ListRules 按优先级返回所有规则
func (s *Store) ListRules() ([]Rule, error) {
	rows, err := s.db.Query(`SELECT id, name, category, alert, keywords, priority FROM rules ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]Rule, 0, 8)
	for rows.Next() {
		var r Rule
		var kwJSON string
		if err := rows.Scan(&r.ID, &r.Name, &r.Category, &r.Alert, &kwJSON, &r.Priority); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(kwJSON), &r.Keywords); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// CountRules 规则总数（用于判断是否需从配置导入）
func (s *Store) CountRules() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM rules`).Scan(&n)
	return n, err
}

// InsertRule 新增规则
func (s *Store) InsertRule(r *Rule) (int64, error) {
	kwJSON, err := json.Marshal(r.Keywords)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO rules (name, category, alert, keywords, priority) VALUES (?, ?, ?, ?, ?)`,
		r.Name, r.Category, r.Alert, string(kwJSON), r.Priority)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateRule 更新规则
func (s *Store) UpdateRule(r *Rule) error {
	kwJSON, err := json.Marshal(r.Keywords)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE rules SET name=?, category=?, alert=?, keywords=?, priority=? WHERE id=?`,
		r.Name, r.Category, r.Alert, string(kwJSON), r.Priority, r.ID)
	return err
}

// DeleteRule 删除规则
func (s *Store) DeleteRule(id int64) error {
	_, err := s.db.Exec(`DELETE FROM rules WHERE id=?`, id)
	return err
}

// Account 持久化的邮箱账号（密码加密存储）
type Account struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Email          string `json:"email"`
	Provider       string `json:"provider"`
	IMAPHost       string `json:"imap_host"`
	IMAPPort       int    `json:"imap_port"`
	AuthType       string `json:"auth_type"`
	AuthUsername   string `json:"auth_username"`
	AuthPasswordEnc string `json:"-"` // 不通过 API 回传
}

// ListAccounts 列出所有账号
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT id, name, email, provider, imap_host, imap_port, auth_type, auth_username, auth_password_enc FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]Account, 0, 4)
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name, &a.Email, &a.Provider, &a.IMAPHost, &a.IMAPPort, &a.AuthType, &a.AuthUsername, &a.AuthPasswordEnc); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// CountAccounts 账号总数（判断是否需从 config 导入）
func (s *Store) CountAccounts() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n)
	return n, err
}

// GetAccount 按名称取账号
func (s *Store) GetAccount(name string) (*Account, error) {
	var a Account
	err := s.db.QueryRow(`SELECT id, name, email, provider, imap_host, imap_port, auth_type, auth_username, auth_password_enc FROM accounts WHERE name=?`, name).
		Scan(&a.ID, &a.Name, &a.Email, &a.Provider, &a.IMAPHost, &a.IMAPPort, &a.AuthType, &a.AuthUsername, &a.AuthPasswordEnc)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAccountByID 按 ID 取账号
func (s *Store) GetAccountByID(id int64) (*Account, error) {
	var a Account
	err := s.db.QueryRow(`SELECT id, name, email, provider, imap_host, imap_port, auth_type, auth_username, auth_password_enc FROM accounts WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Email, &a.Provider, &a.IMAPHost, &a.IMAPPort, &a.AuthType, &a.AuthUsername, &a.AuthPasswordEnc)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// InsertAccount 新增账号（ID 为 0 时新增，否则更新）
func (s *Store) InsertAccount(a *Account) (int64, error) {
	if a.IMAPPort == 0 {
		a.IMAPPort = 993
	}
	if a.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO accounts (name, email, provider, imap_host, imap_port, auth_type, auth_username, auth_password_enc)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			a.Name, a.Email, a.Provider, a.IMAPHost, a.IMAPPort, a.AuthType, a.AuthUsername, a.AuthPasswordEnc)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	_, err := s.db.Exec(`UPDATE accounts SET name=?, email=?, provider=?, imap_host=?, imap_port=?, auth_type=?, auth_username=?, auth_password_enc=? WHERE id=?`,
		a.Name, a.Email, a.Provider, a.IMAPHost, a.IMAPPort, a.AuthType, a.AuthUsername, a.AuthPasswordEnc, a.ID)
	return a.ID, err
}

// DeleteAccount 删除账号
func (s *Store) DeleteAccount(id int64) error {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id=?`, id)
	return err
}
