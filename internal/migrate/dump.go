package migrate

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Dump renders the schema and every row of db as deterministic SQL that
// LoadSQL can replay: PRAGMA user_version, CREATE statements (tables first,
// then indexes/triggers/views, by name), then INSERTs in rowid order. Golden
// fixtures under spec/fixtures/upgrade are stored in this form because SQLite
// file bytes are not stable across library versions.
func Dump(ctx context.Context, db *sql.DB) (string, error) {
	var b strings.Builder
	uv, err := UserVersion(db)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "PRAGMA user_version = %d;\n", uv)
	rows, err := db.QueryContext(ctx, `SELECT type, name, sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 WHEN 'trigger' THEN 2 ELSE 3 END, name`)
	if err != nil {
		return "", err
	}
	type obj struct{ typ, name, sql string }
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.typ, &o.name, &o.sql); err != nil {
			rows.Close()
			return "", err
		}
		objs = append(objs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, o := range objs {
		b.WriteString(strings.TrimSpace(o.sql))
		b.WriteString(";\n")
	}
	for _, o := range objs {
		if o.typ != "table" {
			continue
		}
		if err := dumpRows(ctx, db, o.name, &b); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

func dumpRows(ctx context.Context, db *sql.DB, table string, b *strings.Builder) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s ORDER BY rowid", quoteIdent(table)))
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		lits := make([]string, len(vals))
		for i, v := range vals {
			lits[i] = literal(v)
		}
		fmt.Fprintf(b, "INSERT INTO %s VALUES(%s);\n", quoteIdent(table), strings.Join(lits, ","))
	}
	return rows.Err()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func literal(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case []byte:
		return "X'" + hex.EncodeToString(x) + "'"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	default:
		return "'" + strings.ReplaceAll(fmt.Sprint(x), "'", "''") + "'"
	}
}

// LoadSQL executes a multi-statement script (as produced by Dump) against db.
func LoadSQL(ctx context.Context, db *sql.DB, script string) error {
	_, err := db.ExecContext(ctx, script)
	return err
}
