package store

import "database/sql"

// Additive migrations preserve existing task history and encrypted tailnet state.
func migrate(db *sql.DB) error {
	for _, c := range []struct{ table, name, definition string }{
		{"agents", "run_id", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "last_seen_at", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "reply_to", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "allow_agent_spawn", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.name + " " + c.definition); err != nil {
				return err
			}
		}
	}
	return nil
}
