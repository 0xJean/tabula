package tabula

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// NoArgs is an empty parameter struct for DefineSQL, DefineSQLExec,
// DefineSQLReturning and DefineSQLList declarations that take no named parameters.
type NoArgs struct{}

// SQLSpec declares a named-parameter SQL statement compiled against a Registry.
// Parameters use @name placeholders bound from fields of Args with matching
// `db:"name[,json|zero|array|secret]"` tags.
type SQLSpec struct {
	// Name uniquely identifies this statement in the Registry and snapshots.
	Name string
	// SQL is one PostgreSQL statement using @name placeholders (no positional $1).
	SQL string
	// Scope declares how row scope applies on a scoped Registry. On a scoped
	// Registry (S != struct{}), leaving Scope unset fails closed at Define time.
	// Use ScopeByRLS(), ScopeGlobal(), or ScopeParam("param_name").
	Scope QueryScope
	// AdvisoryLock opts a transaction-level advisory lock SELECT
	// (pg_advisory_xact_lock*, pg_try_advisory_xact_lock*) into DefineSQLExec
	// or marks a DefineSQL lock check with EffectAdvisoryLock (rejected on Direct).
	AdvisoryLock bool
	// Observe configures per-statement logging redaction options (Quiet, Secret).
	Observe []StatementOption
}

type sqlStmtKind uint8

const (
	sqlKindRead sqlStmtKind = iota + 1
	sqlKindExec
	sqlKindReturning
)

type sqlArgField[Args any] struct {
	name         string
	goFieldName  string
	goType       reflect.Type
	index        []int
	field        func(*Args) any
	bind         func(*Args) (any, error)
	secret       bool
	allowedTypes []string
}

type compiledSQLArgs[Args any] struct {
	fields    []sqlArgField[Args]
	byName    map[string]int
	whenByKey map[string]whenField[Args]
	ambiguous map[string]bool
}

type whenField[Args any] struct {
	tagName string // empty for db:"-" fields
	goName  string
	goType  reflect.Type
	active  func(*Args) bool
}

func compileSQLRow[Row, S any](r *Registry[S], name string, fail func(string, ...any) error) (*descriptor[Row, S], []ShapeField, error) {
	rt := reflect.TypeFor[Row]()
	if rt.Kind() != reflect.Struct {
		return nil, nil, fail("row type %s is not a struct", rt)
	}
	v := &View[Row, S]{descriptor: newDescriptor[Row](r, name)}
	var result []ShapeField
	if err := walkFields(rt, nil, "", v.isNestedStruct, func(f reflect.StructField, idx []int, prefix, tag string) error {
		parts := strings.Split(tag, ",")
		rawName := parts[0]
		if rawName == "" {
			return fail("row field %s: db tag has no column name", f.Name)
		}
		colName := prefix + rawName
		options := map[string]bool{}
		for _, opt := range parts[1:] {
			switch opt {
			case "json", "zero", "array", "secret":
				options[opt] = true
			default:
				return fail("row column %s: unsupported option %q", colName, opt)
			}
		}
		switch {
		case options["json"] && options["zero"]:
			return fail("row column %s: option json cannot combine with zero", colName)
		case options["array"] && options["json"]:
			return fail("row column %s: option array cannot combine with json", colName)
		case options["array"] && options["zero"]:
			return fail("row column %s: option array cannot combine with zero", colName)
		}
		wrap, err := r.resolve(f.Type, options["json"], options["zero"], options["array"])
		if err != nil {
			return fail("row column %s: %w", colName, err)
		}
		allowed, err := r.allowedTypes(f.Type, options["json"], options["array"])
		if err != nil {
			return fail("row column %s: %w", colName, err)
		}
		field := fieldAccessor[Row](rt, idx)
		nullable := networkType(f.Type) || f.Type.Kind() == reflect.Pointer || options["zero"] || options["array"]
		if err := v.add(column[Row]{
			name:      colName,
			fieldName: f.Name,
			secret:    options["secret"],
			field:     field,
			goType:    f.Type,
			nullable:  nullable,
			target:    func(row *Row) any { return wrap(field(row)) },
			wrap:      wrap,
			prefix:    prefix,
		}, fail); err != nil {
			return err
		}
		result = append(result, ShapeField{
			Name:         colName,
			Nullable:     nullable,
			AllowedTypes: allowed,
		})
		return nil
	}, fail); err != nil {
		return nil, nil, err
	}
	if len(v.cols) == 0 {
		return nil, nil, fail("row struct %s requires at least one db-tagged column", rt)
	}
	return &v.descriptor, result, nil
}

func compileSQLArgs[Args, S any](r *Registry[S], fail func(string, ...any) error) (*compiledSQLArgs[Args], error) {
	rt := reflect.TypeFor[Args]()
	if rt.Kind() != reflect.Struct {
		return nil, fail("args type %s is not a struct", rt)
	}
	out := &compiledSQLArgs[Args]{
		byName:    make(map[string]int),
		whenByKey: make(map[string]whenField[Args]),
		ambiguous: make(map[string]bool),
	}
	recordWhen := func(key string, wf whenField[Args], prefer bool) {
		if key == "" {
			return
		}
		if existing, ok := out.whenByKey[key]; ok {
			if existing.goName == wf.goName && existing.tagName == wf.tagName {
				return
			}
			if !prefer {
				out.ambiguous[key] = true
				return
			}
		}
		out.whenByKey[key] = wf
	}

	isNested := func(ft reflect.Type) bool {
		if ft.Kind() != reflect.Struct || ft == reflect.TypeFor[time.Time]() {
			return false
		}
		if scanner(reflect.PointerTo(ft)) || reflect.PointerTo(ft).Implements(valuerType) || ft.Implements(valuerType) {
			return false
		}
		r.mu.RLock()
		_, hasType := r.types[ft]
		_, hasZero := r.zeros[ft]
		_, hasDef := r.defaultTypes[ft]
		_, hasFam := r.typeFamilies[ft]
		r.mu.RUnlock()
		return !hasType && !hasZero && !hasDef && !hasFam
	}

	// Collect top-level db:"-" fields as optional When switches (for SQLList).
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if tag, ok := f.Tag.Lookup("db"); ok && tag == "-" {
			if active := buildWhenPredicate[Args](rt, []int{i}, f.Type); active != nil {
				recordWhen(f.Name, whenField[Args]{goName: f.Name, goType: f.Type, active: active}, false)
			}
		}
	}

	if err := walkFields(rt, nil, "", isNested, func(f reflect.StructField, idx []int, prefix, tag string) error {
		parts := strings.Split(tag, ",")
		rawName := parts[0]
		if rawName == "" {
			return fail("args field %s: db tag has no parameter name", f.Name)
		}
		name := prefix + rawName
		if !pgstmt.ValidNamedParam(name) {
			return fail("args field %s: invalid parameter name %q", f.Name, name)
		}
		if _, dup := out.byName[name]; dup {
			return fail("args parameter %q declared twice", name)
		}
		options := map[string]bool{}
		for _, opt := range parts[1:] {
			switch opt {
			case "json", "zero", "array", "secret":
				options[opt] = true
			default:
				return fail("args parameter %q: unsupported option %q", name, opt)
			}
		}
		switch {
		case options["json"] && options["zero"]:
			return fail("args parameter %q: option json cannot combine with zero", name)
		case options["array"] && options["json"]:
			return fail("args parameter %q: option array cannot combine with json", name)
		case options["array"] && options["zero"]:
			return fail("args parameter %q: option array cannot combine with zero", name)
		}
		binder, err := compileArgBinder[Args](r, rt, idx, f.Type, options["json"], options["zero"], options["array"])
		if err != nil {
			return fail("args parameter %q: %w", name, err)
		}
		allowed, err := r.allowedTypes(f.Type, options["json"], options["array"])
		if err != nil {
			return fail("args parameter %q: %w", name, err)
		}
		fieldFn := fieldAccessor[Args](rt, idx)
		out.byName[name] = len(out.fields)
		out.fields = append(out.fields, sqlArgField[Args]{
			name:         name,
			goFieldName:  f.Name,
			goType:       f.Type,
			index:        slices.Clone(idx),
			field:        fieldFn,
			bind:         binder,
			secret:       options["secret"],
			allowedTypes: allowed,
		})
		if active := buildWhenPredicate[Args](rt, idx, f.Type); active != nil {
			wf := whenField[Args]{tagName: name, goName: f.Name, goType: f.Type, active: active}
			recordWhen(name, wf, true)
			if len(idx) == 1 && f.Name != name {
				recordWhen(f.Name, wf, false)
			}
		}
		return nil
	}, fail); err != nil {
		return nil, err
	}
	return out, nil
}

func buildWhenPredicate[Args any](rt reflect.Type, idx []int, ft reflect.Type) func(*Args) bool {
	accessor := fieldAccessor[Args](rt, idx)
	switch ft.Kind() {
	case reflect.Bool:
		return func(a *Args) bool {
			return reflect.ValueOf(accessor(a)).Elem().Bool()
		}
	case reflect.Pointer:
		return func(a *Args) bool {
			return !reflect.ValueOf(accessor(a)).Elem().IsNil()
		}
	case reflect.Slice:
		return func(a *Args) bool {
			return reflect.ValueOf(accessor(a)).Elem().Len() > 0
		}
	case reflect.String:
		return func(a *Args) bool {
			return reflect.ValueOf(accessor(a)).Elem().String() != ""
		}
	default:
		return nil
	}
}

func compileArgBinder[Args, S any](r *Registry[S], rt reflect.Type, idx []int, ft reflect.Type, isJSON, isZero, isArray bool) (func(*Args) (any, error), error) {
	field := fieldAccessor[Args](rt, idx)
	r.mu.RLock()
	defer r.mu.RUnlock()

	switch {
	case isJSON:
		return func(a *Args) (any, error) {
			return JSON(field(a)), nil
		}, nil
	case isZero:
		wrap, ok := r.zeros[ft]
		if !ok {
			return nil, fmt.Errorf("zero option has no registered type %s", ft)
		}
		return func(a *Args) (any, error) {
			return wrap(field(a)), nil
		}, nil
	case isArray:
		if ft.Kind() != reflect.Slice {
			return nil, fmt.Errorf("array option requires a slice type, got %s", ft)
		}
		if r.arrayColFn != nil {
			wrap, err := r.arrayColFn(ft)
			if err != nil {
				return nil, err
			}
			return func(a *Args) (any, error) {
				v := reflect.ValueOf(field(a)).Elem()
				cp := reflect.New(ft)
				cp.Elem().Set(snapshotSliceValue(v))
				return wrap(cp.Interface()), nil
			}, nil
		}
		return func(a *Args) (any, error) {
			v := snapshotSliceValue(reflect.ValueOf(field(a)).Elem()).Interface()
			if r.arrayArgFn != nil {
				return r.arrayArgFn(v), nil
			}
			return v, nil
		}, nil
	}

	if wrap, ok := r.types[ft]; ok {
		return func(a *Args) (any, error) {
			return wrap(field(a)), nil
		}, nil
	}
	if ft.Kind() == reflect.Pointer {
		if wrap, ok := r.types[ft.Elem()]; ok {
			return func(a *Args) (any, error) {
				ptr := reflect.ValueOf(field(a)).Elem()
				if ptr.IsNil() {
					return nil, nil
				}
				return wrap(ptr.Interface()), nil
			}, nil
		}
	}
	if def, ok := r.defaultTypes[ft]; ok && def.kind.encoder != nil {
		enc := def.kind.encoder
		return func(a *Args) (any, error) {
			return enc.encode(reflect.ValueOf(field(a)).Elem().Interface())
		}, nil
	}
	if ft.Kind() == reflect.Pointer {
		if def, ok := r.defaultTypes[ft.Elem()]; ok && def.kind.encoder != nil {
			enc := def.kind.encoder
			return func(a *Args) (any, error) {
				ptr := reflect.ValueOf(field(a)).Elem()
				if ptr.IsNil() {
					return nil, nil
				}
				return enc.encode(ptr.Elem().Interface())
			}, nil
		}
	}

	base := ft
	isPtr := base.Kind() == reflect.Pointer
	if isPtr {
		base = base.Elem()
	}
	if base.Implements(valuerType) || reflect.PointerTo(base).Implements(valuerType) {
		if isPtr {
			return func(a *Args) (any, error) {
				ptr := reflect.ValueOf(field(a)).Elem()
				if ptr.IsNil() {
					return nil, nil
				}
				if base.Implements(valuerType) {
					return ptr.Elem().Interface(), nil
				}
				return ptr.Interface(), nil
			}, nil
		}
		if base.Implements(valuerType) {
			return func(a *Args) (any, error) {
				return reflect.ValueOf(field(a)).Elem().Interface(), nil
			}, nil
		}
		return func(a *Args) (any, error) {
			return field(a), nil
		}, nil
	}

	supported := false
	switch base.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		supported = true
	case reflect.Slice:
		supported = base.Elem().Kind() == reflect.Uint8
	case reflect.Struct:
		supported = base == timeType
	}
	if !supported {
		return nil, fmt.Errorf("unsupported Go argument type %s (implement driver.Valuer, register with RegisterType/DefaultTypes, or use json/zero/array)", ft)
	}
	if isPtr {
		return func(a *Args) (any, error) {
			ptr := reflect.ValueOf(field(a)).Elem()
			if ptr.IsNil() {
				return nil, nil
			}
			val := ptr.Elem()
			if base.Kind() == reflect.Slice && base.Elem().Kind() == reflect.Uint8 {
				return snapshotSliceValue(val).Interface(), nil
			}
			return val.Interface(), nil
		}, nil
	}
	if base.Kind() == reflect.Slice && base.Elem().Kind() == reflect.Uint8 {
		return func(a *Args) (any, error) {
			return snapshotSliceValue(reflect.ValueOf(field(a)).Elem()).Interface(), nil
		}, nil
	}
	return func(a *Args) (any, error) {
		return reflect.ValueOf(field(a)).Elem().Interface(), nil
	}, nil
}

func snapshotSliceValue(v reflect.Value) reflect.Value {
	if v.IsNil() {
		return v
	}
	cp := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
	reflect.Copy(cp, v)
	return cp
}

type resolvedSQLScope struct {
	mode  scopeMode
	param string
	types []string
}

func resolveSQLScope[S any](r *Registry[S], scope QueryScope, fail func(string, ...any) error) (resolvedSQLScope, error) {
	mode := scope.mode
	unscopedRegistry := r.scopeType == reflect.TypeFor[struct{}]()
	if mode == scopeUnset {
		if !unscopedRegistry {
			return resolvedSQLScope{}, fail("scoped registry requires explicit Scope (ScopeByRLS, ScopeGlobal or ScopeParam)")
		}
		mode = scopeGlobal
	}
	switch mode {
	case scopeByRLS, scopeGlobal:
		return resolvedSQLScope{mode: mode}, nil
	case scopeColumns:
		return resolvedSQLScope{}, fail("ScopeColumns is not supported on DefineSQL* (use ScopeParam, ScopeByRLS or ScopeGlobal)")
	case scopeParam:
		if unscopedRegistry {
			return resolvedSQLScope{}, fail("ScopeParam requires a scoped registry (S != struct{})")
		}
		name := strings.TrimSpace(scope.param)
		if !pgstmt.ValidNamedParam(name) {
			return resolvedSQLScope{}, fail("ScopeParam requires a valid identifier name, got %q", scope.param)
		}
		types, err := r.scopeAllowedTypes()
		if err != nil {
			return resolvedSQLScope{}, fail("ScopeParam %q: %w", name, err)
		}
		return resolvedSQLScope{mode: scopeParam, param: name, types: types}, nil
	default:
		return resolvedSQLScope{}, fail("unsupported Scope mode")
	}
}

func (r *Registry[S]) checkSQLGuards(query string) error {
	if err := GuardNoSessionEscape(query); err != nil {
		return err
	}
	if r.sessionGuard != nil {
		if err := r.sessionGuard(query); err != nil {
			return err
		}
	}
	return nil
}

type sqlClassification struct {
	effects     ShapeEffects
	stmtEffects statementEffects
	fixedMethod CallMethod
}

func classifySQLStatement(query string, kind sqlStmtKind, advisoryLock bool) (sqlClassification, error) {
	tokens, ok := pgstmt.SingleDML(query)
	if !ok {
		return sqlClassification{}, errors.New("SQL must be a single valid PostgreSQL data statement")
	}
	stmt := statementHead(tokens)
	var bodies [][]pgstmt.Token
	main := outerCommand(stmt, &bodies)
	if len(main) == 0 {
		return sqlClassification{}, errors.New("unsupported SQL statement or CTE form")
	}
	outerCmd := main[0].Text
	if readCommand(outerCmd) && hasClause(main, "into") {
		return sqlClassification{}, errors.New("SELECT INTO is not allowed")
	}

	writeCmdSet := make(map[string]bool)
	hasModifyingCTE := false
	pending := slices.Clone(bodies)
	for len(pending) > 0 {
		last := len(pending) - 1
		body := pending[last]
		pending = pending[:last]
		cmd := outerCommand(statementHead(body), &pending)
		if len(cmd) == 0 {
			return sqlClassification{}, errors.New("unsupported CTE body")
		}
		if readCommand(cmd[0].Text) {
			if hasClause(cmd, "into") {
				return sqlClassification{}, errors.New("SELECT INTO inside CTE is not allowed")
			}
			continue
		}
		if writeCommand(cmd[0].Text) {
			hasModifyingCTE = true
			writeCmdSet[strings.ToUpper(cmd[0].Text)] = true
			continue
		}
		return sqlClassification{}, fmt.Errorf("unsupported CTE command %q", cmd[0].Text)
	}
	outerWrite := writeCommand(outerCmd)
	if outerWrite {
		writeCmdSet[strings.ToUpper(outerCmd)] = true
	}
	hasReturning := outerWrite && hasClause(main, "returning")
	hasLock := hasRowLock(tokens)
	hasXactLock, hasSessionLock := scanAdvisoryLocks(tokens)
	if hasSessionLock {
		return sqlClassification{}, errors.New("session-level advisory lock functions are not allowed; use transaction-level pg_*advisory_xact_lock*")
	}

	writes := make([]statementWrite, 0, len(writeCmdSet))
	for _, cmd := range slices.Sorted(maps.Keys(writeCmdSet)) {
		writes = append(writes, statementWrite{command: cmd})
	}

	switch kind {
	case sqlKindRead:
		if outerWrite || hasModifyingCTE {
			return sqlClassification{}, errors.New("DefineSQL accepts read-only queries only; use DefineSQLExec or DefineSQLReturning for writes")
		}
		if advisoryLock && !hasXactLock {
			return sqlClassification{}, errors.New("AdvisoryLock requires a transaction-level pg_[try_]advisory_xact_lock[_shared] call")
		}
		eff := EffectRead
		if hasLock {
			eff |= EffectRowLock
		}
		if advisoryLock || hasXactLock {
			eff |= EffectAdvisoryLock
		}
		return sqlClassification{
			effects:     eff,
			stmtEffects: statementEffects{bits: eff},
		}, nil

	case sqlKindExec:
		if advisoryLock {
			if !isStandaloneXactAdvisoryLock(tokens) {
				return sqlClassification{}, errors.New("DefineSQLExec with AdvisoryLock requires a standalone SELECT calling pg_[try_]advisory_xact_lock[_shared](...)")
			}
			eff := EffectRead | EffectAdvisoryLock
			return sqlClassification{
				effects:     eff,
				stmtEffects: statementEffects{bits: eff},
				fixedMethod: CallEach,
			}, nil
		}
		if !outerWrite {
			if hasModifyingCTE {
				return sqlClassification{}, errors.New("modifying CTE returning rows must use DefineSQLReturning (or end with an outer INSERT/UPDATE/DELETE/MERGE without RETURNING)")
			}
			return sqlClassification{}, errors.New("DefineSQLExec requires a write statement (INSERT/UPDATE/DELETE/MERGE) or AdvisoryLock: true")
		}
		if hasReturning {
			return sqlClassification{}, errors.New("DefineSQLExec rejects statements with top-level RETURNING; use DefineSQLReturning")
		}
		return sqlClassification{
			effects: EffectWrite,
			stmtEffects: statementEffects{
				bits:     EffectWrite,
				writes:   writes,
				writeCTE: hasModifyingCTE,
			},
		}, nil

	case sqlKindReturning:
		if advisoryLock {
			return sqlClassification{}, errors.New("DefineSQLReturning does not accept AdvisoryLock; use DefineSQL or DefineSQLExec")
		}
		if outerWrite {
			if !hasReturning {
				return sqlClassification{}, errors.New("DefineSQLReturning requires a top-level RETURNING clause or a modifying CTE returning rows; use DefineSQLExec for count-only writes")
			}
		} else if !hasModifyingCTE {
			return sqlClassification{}, errors.New("DefineSQLReturning requires a write statement; use DefineSQL for read-only queries")
		}
		return sqlClassification{
			effects: EffectWrite,
			stmtEffects: statementEffects{
				bits:     EffectWrite,
				writes:   writes,
				writeCTE: hasModifyingCTE,
			},
		}, nil

	default:
		return sqlClassification{}, errors.New("unknown SQL declaration kind")
	}
}

func hasRowLock(tokens []pgstmt.Token) bool {
	for i := 0; i < len(tokens); i++ {
		if tokens[i] != (pgstmt.Token{Kind: pgstmt.Word, Text: "for"}) {
			continue
		}
		if i > 0 && (tokens[i-1] == (pgstmt.Token{Kind: pgstmt.Symbol, Text: "."}) ||
			tokens[i-1] == (pgstmt.Token{Kind: pgstmt.Word, Text: "as"})) {
			continue
		}
		if isWord(tokens[i+1:], "update") || isWord(tokens[i+1:], "share") ||
			(isWord(tokens[i+1:], "key") && isWord(tokens[i+2:], "share")) ||
			(isWord(tokens[i+1:], "no") && isWord(tokens[i+2:], "key") && isWord(tokens[i+3:], "update")) {
			return true
		}
	}
	return false
}

func isXactAdvisoryLockFunc(name string) bool {
	switch name {
	case "pg_advisory_xact_lock", "pg_advisory_xact_lock_shared",
		"pg_try_advisory_xact_lock", "pg_try_advisory_xact_lock_shared":
		return true
	default:
		return false
	}
}

func isSessionAdvisoryLockFunc(name string) bool {
	switch name {
	case "pg_advisory_lock", "pg_advisory_lock_shared",
		"pg_try_advisory_lock", "pg_try_advisory_lock_shared",
		"pg_advisory_unlock", "pg_advisory_unlock_shared", "pg_advisory_unlock_all":
		return true
	default:
		return false
	}
}

func scanAdvisoryLocks(tokens []pgstmt.Token) (hasXact, hasSession bool) {
	for i := 0; i < len(tokens)-1; i++ {
		if tokens[i].Kind != pgstmt.Word && tokens[i].Kind != pgstmt.Quoted {
			continue
		}
		if tokens[i+1] != (pgstmt.Token{Kind: pgstmt.Symbol, Text: "("}) {
			continue
		}
		name := strings.ToLower(tokens[i].Text)
		if isXactAdvisoryLockFunc(name) {
			hasXact = true
		}
		if isSessionAdvisoryLockFunc(name) {
			hasSession = true
		}
	}
	return hasXact, hasSession
}

func isStandaloneXactAdvisoryLock(tokens []pgstmt.Token) bool {
	if len(tokens) < 4 || !isWord(tokens, "select") {
		return false
	}
	rest := tokens[1:]
	if len(rest) >= 2 && (rest[0].Kind == pgstmt.Word || rest[0].Kind == pgstmt.Quoted) &&
		strings.EqualFold(rest[0].Text, "pg_catalog") && isSymbol(rest[1:], ".") {
		rest = rest[2:]
	}
	if len(rest) < 3 || (rest[0].Kind != pgstmt.Word && rest[0].Kind != pgstmt.Quoted) ||
		!isXactAdvisoryLockFunc(strings.ToLower(rest[0].Text)) {
		return false
	}
	body, tail := takeGroup(rest[1:])
	if len(body) == 0 || len(tail) != 0 {
		return false
	}
	for _, tok := range body {
		if tok.Kind == pgstmt.Word && (readCommand(tok.Text) || writeCommand(tok.Text) || tok.Text == "with") {
			return false
		}
	}
	return true
}

func (r *Registry[S]) recordSQLDefinition(def Definition) (*registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.views {
		if existing.def.Name == def.Name {
			return nil, fmt.Errorf("%s: SQL declaration %q already registered", r.prefix, def.Name)
		}
	}
	e := &registration{def: cloneDefinition(def), bySQL: make(map[string]stmtMeta)}
	r.views = append(r.views, e)
	return e, nil
}
