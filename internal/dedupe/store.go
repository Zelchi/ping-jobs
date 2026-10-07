package dedupe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
)

const retention = 30 * 24 * time.Hour

var trackingKeys = map[string]struct{}{
	"action": {}, "fbclid": {}, "gclid": {}, "gh_src": {}, "jobboardsource": {},
	"lever-source": {}, "ref": {}, "referral": {}, "source": {}, "sourcetoken": {},
	"utm_campaign": {}, "utm_content": {}, "utm_medium": {}, "utm_source": {}, "utm_term": {},
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("caminho do banco SQLite não pode ser vazio")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("criar diretório do banco SQLite: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("abrir banco SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Accept(ctx context.Context, title, link string, enqueue func() error) (bool, error) {
	if enqueue == nil {
		return false, fmt.Errorf("função de enfileiramento não pode ser vazia")
	}

	titleKey := normalizeTitle(title)
	canonicalURL, err := canonicalizeURL(link)
	if err != nil {
		return false, fmt.Errorf("normalizar URL da vaga: %w", err)
	}

	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(opCtx, nil)
	if err != nil {
		return false, fmt.Errorf("iniciar transação SQLite: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	nowUnix := now.UnixNano()
	if _, err := tx.ExecContext(opCtx, `DELETE FROM sent_works WHERE expires_at <= ?`, nowUnix); err != nil {
		return false, fmt.Errorf("limpar deduplicação expirada: %w", err)
	}

	matchedBy, err := findMatch(opCtx, tx, nowUnix, titleKey, canonicalURL)
	if err != nil {
		return false, fmt.Errorf("consultar duplicidade no SQLite: %w", err)
	}
	if matchedBy != "" {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("confirmar limpeza do SQLite: %w", err)
		}
		return true, nil
	}

	if err := enqueue(); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(opCtx, `
		INSERT INTO sent_works (title, link, title_key, canonical_url, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, title, link, titleKey, canonicalURL, nowUnix, now.Add(retention).UnixNano()); err != nil {
		return false, fmt.Errorf("registrar vaga no SQLite: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("confirmar registro no SQLite: %w", err)
	}
	return false, nil
}

func (s *Store) Check(ctx context.Context, title, link string) (bool, string, error) {
	titleKey := normalizeTitle(title)
	canonicalURL, err := canonicalizeURL(link)
	if err != nil {
		return false, "", fmt.Errorf("normalizar URL da vaga: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	matchedBy, err := findMatch(ctx, s.db, time.Now().UnixNano(), titleKey, canonicalURL)
	if err != nil {
		return false, "", fmt.Errorf("consultar duplicidade no SQLite: %w", err)
	}
	return matchedBy != "", matchedBy, nil
}

func findMatch(ctx context.Context, queryer rowQuerier, nowUnix int64, titleKey, canonicalURL string) (string, error) {
	if canonicalURL != "" {
		var found int
		err := queryer.QueryRowContext(ctx, `
			SELECT 1
			FROM sent_works
			WHERE expires_at > ? AND canonical_url = ?
			LIMIT 1`, nowUnix, canonicalURL).Scan(&found)
		if err == nil {
			return "url", nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}

	if titleKey != "" {
		var found int
		err := queryer.QueryRowContext(ctx, `
			SELECT 1
			FROM sent_works
			WHERE expires_at > ? AND title_key = ?
			LIMIT 1`, nowUnix, titleKey).Scan(&found)
		if err == nil {
			return "title", nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", nil
}

func (s *Store) PruneExpired(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `DELETE FROM sent_works WHERE expires_at <= ?`, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("limpar vagas expiradas do SQLite: %w", err)
	}
	return nil
}

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("configurar espera do SQLite: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("configurar journal do SQLite: %w", err)
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS sent_works (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			link TEXT NOT NULL,
			title_key TEXT NOT NULL,
			canonical_url TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS sent_works_url_expiry_idx ON sent_works (canonical_url, expires_at)`,
		`CREATE INDEX IF NOT EXISTS sent_works_title_expiry_idx ON sent_works (title_key, expires_at)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("criar estrutura do SQLite: %w", err)
		}
	}
	return s.PruneExpired(ctx)
}

func normalizeTitle(value string) string {
	value = cases.Fold().String(norm.NFKC.String(value))
	var normalized strings.Builder
	space := true
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsNumber(char) || char == '_' {
			if space && normalized.Len() > 0 {
				normalized.WriteByte(' ')
			}
			normalized.WriteRune(char)
			space = false
			continue
		}
		if !space && normalized.Len() > 0 {
			normalized.WriteByte(' ')
		}
		space = true
	}
	return strings.TrimSpace(normalized.String())
}

func canonicalizeURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("URL HTTP ou HTTPS inválida")
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	parsed.RawFragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	parsed.RawPath = ""

	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", err
	}
	for key := range query {
		if _, tracking := trackingKeys[strings.ToLower(key)]; tracking {
			delete(query, key)
			continue
		}
		sort.Strings(query[key])
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
