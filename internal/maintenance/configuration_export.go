package maintenance

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Configuration backups copy selected rows directly into a new database. They
// never snapshot all control history and delete it afterwards. The attached
// source is read-only and all reads share one SQLite transaction, including WAL.
func exportConfiguration(source, destination string) error {
	if err := requireRegularFile(source); err != nil {
		return err
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	sourceURL := url.URL{Scheme: "file", Path: absolute}
	query := sourceURL.Query()
	query.Set("mode", "ro")
	sourceURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", destination)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("ATTACH DATABASE ? AS original", sourceURL.String()); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT type,name,sql FROM original.sqlite_schema
  WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
  ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 WHEN 'view' THEN 2 ELSE 3 END,name`)
	if err != nil {
		return err
	}
	type object struct{ kind, name, sql string }
	var objects []object
	for rows.Next() {
		var o object
		if err = rows.Scan(&o.kind, &o.name, &o.sql); err != nil {
			rows.Close()
			return err
		}
		objects = append(objects, o)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, o := range objects {
		if o.kind == "table" {
			if _, err = tx.Exec(o.sql); err != nil {
				return fmt.Errorf("create configuration table %s: %w", o.name, err)
			}
		}
	}
	// Unlisted tables retain their schemas but no history or ephemeral state.
	filters := map[string]string{
		"control_config_versions": " WHERE active=1",
		"local_secrets":           "", "api_keys": "", "api_key_secrets": "",
		"management_administrator": "", "management_tokens": "", "runtime_settings": "",
		"provider_capability_evidence": "", "policies": "",
		"policy_set_versions": " WHERE config_version IN (SELECT version FROM original.control_config_versions WHERE active=1)",
		"policy_set_members":  " WHERE set_version IN (SELECT version FROM original.policy_set_versions WHERE config_version IN (SELECT version FROM original.control_config_versions WHERE active=1))",
		"policy_revisions": ` WHERE EXISTS (SELECT 1 FROM original.policies p WHERE p.id=original.policy_revisions.policy_id AND p.active_revision=original.policy_revisions.revision)
   OR EXISTS (SELECT 1 FROM original.policy_set_members m JOIN original.policy_set_versions s ON s.version=m.set_version JOIN original.control_config_versions c ON c.version=s.config_version AND c.active=1
   WHERE m.policy_id=original.policy_revisions.policy_id AND m.policy_revision=original.policy_revisions.revision)`,
		"configuration_revision_audit": " WHERE revision IN (SELECT version FROM original.control_config_versions WHERE active=1)",
	}
	for _, o := range objects {
		filter, keep := filters[o.name]
		if o.kind != "table" || !keep {
			continue
		}
		name := `"` + strings.ReplaceAll(o.name, `"`, `""`) + `"`
		// #nosec G202 -- identifiers are quoted, and filters come only from the fixed table allowlist above.
		if _, err = tx.Exec("INSERT INTO main." + name + " SELECT * FROM original." + name + filter); err != nil {
			return fmt.Errorf("export configuration table %s: %w", o.name, err)
		}
	}
	for _, o := range objects {
		if o.kind != "table" {
			if _, err = tx.Exec(o.sql); err != nil {
				return fmt.Errorf("create configuration schema %s: %w", o.name, err)
			}
		}
	}
	// Preserve monotonically increasing IDs after older versions are omitted.
	var sequences int
	if err = tx.QueryRow("SELECT count(*) FROM original.sqlite_schema WHERE name='sqlite_sequence'").Scan(&sequences); err != nil {
		return err
	}
	if sequences > 0 {
		if _, err = tx.Exec(`DELETE FROM main.sqlite_sequence; INSERT INTO main.sqlite_sequence SELECT * FROM original.sqlite_sequence`); err != nil {
			return err
		}
	}
	check, err := tx.Query("PRAGMA main.foreign_key_check")
	if err != nil {
		return err
	}
	invalid := check.Next()
	err = check.Err()
	check.Close()
	if err != nil {
		return err
	}
	if invalid {
		return fmt.Errorf("configuration export contains invalid references")
	}
	return tx.Commit()
}

// The scope declaration is a contract: a configuration backup cannot silently
// contain request history even if its checksums and SQLite schema are valid.
func verifyEmptyHistory(path string) error {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query("SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, name := range names {
		quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		var exists int
		// #nosec G202 -- identifiers are quoted schema names from an already verified backup database.
		if err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM " + quoted + " LIMIT 1)").Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return fmt.Errorf("configuration backup contains history in %s", filepath.Base(path))
		}
	}
	return nil
}
