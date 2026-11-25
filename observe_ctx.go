package tabula

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// ObserveMask identifies which observability signals a runner session handles
// itself so that a Registry skips duplicate emissions for those signals.
type ObserveMask uint8

const (
	ObserveTrace ObserveMask = 1 << iota
	ObserveMetrics
	ObserveLog
	// ObserveTag indicates the session tags statements with SQL comments and
	// needs StatementMeta propagated on the context even without other hooks.
	ObserveTag
)

// StatementMeta carries descriptor statement metadata from a Table, View or
// Query binding down to a session-level Observer via context.
type StatementMeta struct {
	Key        string
	Table      string
	Action     string
	Scope      string
	SecretArgs []bool
	AllSecret  bool
	Quiet      bool
	Logger     *slog.Logger
	NoRowsOK   bool
}

type stmtMetaKey struct{}

// WithStatementMeta attaches descriptor statement metadata to ctx for a
// session-level Observer to consume.
func WithStatementMeta(ctx context.Context, meta StatementMeta) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, stmtMetaKey{}, meta)
}

// StatementMetaFrom extracts descriptor statement metadata previously attached
// with WithStatementMeta.
func StatementMetaFrom(ctx context.Context) (StatementMeta, bool) {
	if ctx == nil {
		return StatementMeta{}, false
	}
	m, ok := ctx.Value(stmtMetaKey{}).(StatementMeta)
	return m, ok
}

// ObserverConfig configures a session-level Observer shared by driver runners.
type ObserverConfig struct {
	Role    string
	Tracer  Tracer
	Metrics Metrics
	Log     LogConfig
	// Sanitize supplies reviewed log text for errors. The default is the error class.
	Sanitize      func(error) string
	TagStatements bool
}

// Observer implements session-level tracing, metrics, structured logging and
// optional SQL comment tagging for both descriptor statements and hand-written
// inline SQL.
type Observer struct {
	role     string
	tracer   Tracer
	metrics  Metrics
	log      *logState
	sanitize func(error) string
	tagSQL   bool
	signals  ObserveMask
}

// NewObserver constructs a session-level Observer from cfg. Returns nil when no
// observability signal or statement tagging is configured.
func NewObserver(cfg ObserverConfig) *Observer {
	var signals ObserveMask
	if cfg.Tracer != nil {
		signals |= ObserveTrace
	}
	if cfg.Metrics != nil {
		signals |= ObserveMetrics
	}
	var ls *logState
	if cfg.Log.Logger != nil && (cfg.Log.LevelVar != nil || cfg.Log.Level != LogOff) {
		ls = newLogState(cfg.Log)
		signals |= ObserveLog
	}
	if cfg.TagStatements {
		signals |= ObserveTag
	}
	if signals == 0 {
		return nil
	}
	if ls == nil {
		ls = newLogState(cfg.Log)
	}
	return &Observer{
		role:     cfg.Role,
		tracer:   cfg.Tracer,
		metrics:  cfg.Metrics,
		log:      ls,
		sanitize: cfg.Sanitize,
		tagSQL:   cfg.TagStatements,
		signals:  signals,
	}
}

// Signals reports which observability signals this Observer owns.
func (o *Observer) Signals() ObserveMask {
	if o == nil {
		return 0
	}
	return o.signals
}

// TagSQL returns query prefixed with `/* tabula:<key> */ ` when TagStatements
// is enabled. Key comes from descriptor metadata when present or from the
// normalized inline SQL fingerprint otherwise.
func (o *Observer) TagSQL(ctx context.Context, query string) string {
	if o == nil || !o.tagSQL {
		return query
	}
	key := o.statementKey(ctx, query)
	return TagSQL(query, key)
}

// TagSQL prefixes query with a sanitized `/* tabula:<key> */ ` comment.
func TagSQL(query, key string) string {
	if key == "" {
		return query
	}
	safe := strings.ReplaceAll(strings.ReplaceAll(key, "/*", "_"), "*/", "_")
	safe = strings.ReplaceAll(strings.ReplaceAll(safe, "\n", " "), "\r", " ")
	return "/* tabula:" + safe + " */ " + query
}

func (o *Observer) statementKey(ctx context.Context, query string) string {
	if meta, ok := StatementMetaFrom(ctx); ok && meta.Key != "" {
		return meta.Key
	}
	return "tabula.inline." + SQLFingerprint(query)
}

// Start begins observing one session-level statement execution and returns the
// updated context plus a finish callback that records row count and error.
func (o *Observer) Start(ctx context.Context, scopeLabel, query string, args []any) (context.Context, func(rows int64, err error)) {
	if o == nil || o.signals == 0 {
		return ctx, func(int64, error) {}
	}
	meta, hasMeta := StatementMetaFrom(ctx)
	key := meta.Key
	action := meta.Action
	table := meta.Table
	if !hasMeta || key == "" {
		key = "tabula.inline." + SQLFingerprint(query)
	}
	if action == "" {
		action = SQLAction(query)
	}
	scope := scopeLabel
	if scope == "" {
		scope = meta.Scope
	}
	info := TraceInfo{
		Key:    key,
		Table:  table,
		Action: action,
		Scope:  scope,
		Role:   o.role,
	}
	logger := o.log.logger
	if meta.Logger != nil && o.signals&ObserveLog != 0 {
		logger = meta.Logger
	}
	logActive := o.signals&ObserveLog != 0 && logger != nil && o.log.level.Level() != LogOff
	var start time.Time
	if logActive || o.metrics != nil {
		start = o.log.now()
	}
	var endTrace func(int64, error)
	if o.tracer != nil {
		ctx, endTrace = o.tracer.Start(ctx, info)
	}
	return ctx, func(rows int64, err error) {
		if errors.Is(err, stopIteration) {
			err = nil
			if rows >= 0 {
				rows++
			}
		} else if hasMeta && meta.NoRowsOK {
			if errors.Is(err, sql.ErrNoRows) {
				err = nil
				rows = 0
			} else if err == nil && rows < 0 {
				rows = 1
			}
		}
		var dur time.Duration
		if !start.IsZero() {
			dur = o.log.now().Sub(start)
		}
		if endTrace != nil {
			endTrace(rows, traceError(ctx, err))
		}
		if o.metrics != nil {
			var class ErrorClass
			if err != nil {
				class, _ = ClassifyError(ctx, err)
			}
			o.metrics.Observe(ctx, info, dur, rows, class)
		}
		if logActive {
			emitLog(ctx, o.log, logger, o.sanitize, scope, o.role, logEvent{
				key:       key,
				table:     table,
				action:    action,
				sql:       query,
				duration:  dur,
				rows:      rows,
				err:       err,
				args:      args,
				secretArg: meta.SecretArgs,
				allSecret: meta.AllSecret,
				quiet:     meta.Quiet,
			})
		}
	}
}

// SQLFingerprint returns a stable 12-character hex fingerprint of normalized
// PostgreSQL text. Literal values, numeric constants and numbered parameters
// are replaced by "?" so argument values never enter the fingerprint.
func SQLFingerprint(query string) string {
	toks := pgstmt.Lex(query)
	var b strings.Builder
	for i := 0; i < len(toks); i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		tok := toks[i]
		switch {
		case tok.Kind == pgstmt.Literal:
			b.WriteByte('?')
		case tok.Kind == pgstmt.Symbol && len(tok.Text) > 1 && tok.Text[0] == '$':
			b.WriteByte('?')
		case tok.Kind == pgstmt.Symbol && isNumericLiteral(tok.Text):
			// pgstmt.Lex emits digits as individual Symbol tokens; collapse
			// multi-digit integers and decimals into a single "?" placeholder.
			for i+1 < len(toks) && toks[i+1].Kind == pgstmt.Symbol && isNumericLiteral(toks[i+1].Text) {
				i++
			}
			if i+2 < len(toks) && toks[i+1] == (pgstmt.Token{Kind: pgstmt.Symbol, Text: "."}) &&
				toks[i+2].Kind == pgstmt.Symbol && isNumericLiteral(toks[i+2].Text) {
				i += 2
				for i+1 < len(toks) && toks[i+1].Kind == pgstmt.Symbol && isNumericLiteral(toks[i+1].Text) {
					i++
				}
			}
			b.WriteByte('?')
		default:
			b.WriteString(tok.Text)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:6])
}

func isNumericLiteral(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// SQLAction infers a low-cardinality verb label from query for inline SQL
// telemetry ("select", "insert", "update", "delete", "merge", "with", or "sql").
func SQLAction(query string) string {
	toks := pgstmt.Lex(query)
	for _, tok := range toks {
		if tok.Kind == pgstmt.Symbol && tok.Text == "(" {
			continue
		}
		if tok.Kind != pgstmt.Word {
			return "sql"
		}
		switch tok.Text {
		case "select", "insert", "update", "delete", "merge", "values", "table":
			return tok.Text
		case "with":
			// Scan for top-level DML verb after the CTE list if possible.
			depth := 0
			for _, t := range toks[1:] {
				if t.Kind == pgstmt.Symbol {
					switch t.Text {
					case "(":
						depth++
					case ")":
						if depth > 0 {
							depth--
						}
					}
					continue
				}
				if depth == 0 && t.Kind == pgstmt.Word {
					switch t.Text {
					case "select", "insert", "update", "delete", "merge":
						return t.Text
					}
				}
			}
			return "with"
		default:
			return "sql"
		}
	}
	return "sql"
}
