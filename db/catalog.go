package db

import "github.com/rohanthewiz/serr"

// TablesQuery returns the statement that lists a driver's tables and views —
// the `\dt` of whichever database this is. The shape is deliberately the same
// across drivers (schema/owner, name, type), so the results table reads the
// same wherever you press the key.
//
// It is ordinary SQL run through the ordinary path, which means it is
// cancelable, capped by max_rows, and exportable like any other result.
func TablesQuery(driver string) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	switch drv {
	case "pgx":
		// the two catalog schemas are always there and never what was meant
		return `SELECT table_schema, table_name, table_type
FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
ORDER BY table_schema, table_name`, nil
	case "mysql":
		// DATABASE() scopes this to the schema the DSN connected to, rather
		// than every schema on the server
		return `SELECT table_schema, table_name, table_type
FROM information_schema.tables
WHERE table_schema = DATABASE()
ORDER BY table_name`, nil
	case "sqlite":
		// sqlite has no schemas; the literal keeps the column count matching
		// the other drivers. The sqlite_ prefix is reserved for internals.
		return `SELECT 'main' AS table_schema, name AS table_name, upper(type) AS table_type
FROM sqlite_master
WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%'
ORDER BY type, name`, nil
	}
	return "", serr.New("no table listing for this driver", "driver", driver)
}
