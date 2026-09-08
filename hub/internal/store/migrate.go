package store

import "database/sql"
import "github.com/scs32/tailterm/hub/internal/api"

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

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS profile_meta (key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS profiles(username TEXT PRIMARY KEY,key_hash TEXT NOT NULL,revision INTEGER NOT NULL,updated_at TEXT NOT NULL,envelope BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS profile_history(username TEXT NOT NULL,revision INTEGER NOT NULL,updated_at TEXT NOT NULL,envelope BLOB NOT NULL,PRIMARY KEY(username,revision));`); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO profile_meta(key,value) VALUES('instance',?)`, api.NewID("profilehub"))
	return err
}
