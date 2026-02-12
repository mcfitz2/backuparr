package backup

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// PostgresConfig holds Postgres connection details extracted from config.xml
type PostgresConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	MainDB   string
	LogDB    string
}

// ConfigXML represents the XML structure of config.xml
type ConfigXML struct {
	XMLName          xml.Name `xml:"Config"`
	PostgresUser     string   `xml:"PostgresUser"`
	PostgresPassword string   `xml:"PostgresPassword"`
	PostgresPort     string   `xml:"PostgresPort"`
	PostgresHost     string   `xml:"PostgresHost"`
	PostgresMainDb   string   `xml:"PostgresMainDb"`
	PostgresLogDb    string   `xml:"PostgresLogDb"`
}

// ParsePostgresConfigFromZip extracts Postgres connection details from config.xml in a backup zip
func ParsePostgresConfigFromZip(zipData []byte) (*PostgresConfig, error) {
	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("failed to open zip: %w", err)
	}

	for _, file := range reader.File {
		if file.Name == "config.xml" {
			rc, err := file.Open()
			if err != nil {
				return nil, fmt.Errorf("failed to open config.xml: %w", err)
			}
			defer rc.Close()

			data, err := io.ReadAll(rc)
			if err != nil {
				return nil, fmt.Errorf("failed to read config.xml: %w", err)
			}

			var config ConfigXML
			if err := xml.Unmarshal(data, &config); err != nil {
				return nil, fmt.Errorf("failed to parse config.xml: %w", err)
			}

			// If no Postgres config, return nil (SQLite mode)
			if config.PostgresHost == "" {
				return nil, nil
			}

			return &PostgresConfig{
				Host:     config.PostgresHost,
				Port:     config.PostgresPort,
				User:     config.PostgresUser,
				Password: config.PostgresPassword,
				MainDB:   config.PostgresMainDb,
				LogDB:    config.PostgresLogDb,
			}, nil
		}
	}

	return nil, fmt.Errorf("config.xml not found in backup")
}

// DumpDatabase runs pg_dump and returns the SQL dump as bytes
func (c *PostgresConfig) DumpDatabase(dbName string) ([]byte, error) {
	if dbName == "" {
		return nil, fmt.Errorf("database name is empty")
	}

	// Build connection string for pg_dump
	// Format: pg_dump -h host -p port -U user -d dbname
	args := []string{
		"-h", c.Host,
		"-p", c.Port,
		"-U", c.User,
		"-d", dbName,
		"--no-password", // We'll use PGPASSWORD env var
		"--format=plain",
		"--no-owner",
		"--no-acl",
	}

	cmd := exec.Command("pg_dump", args...)
	cmd.Env = append(cmd.Environ(), fmt.Sprintf("PGPASSWORD=%s", c.Password))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pg_dump failed: %w - %s", err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// DumpAllDatabases dumps both main and log databases
func (c *PostgresConfig) DumpAllDatabases() (map[string][]byte, error) {
	dumps := make(map[string][]byte)

	databases := []string{c.MainDB, c.LogDB}
	for _, db := range databases {
		if db == "" {
			continue
		}

		dump, err := c.DumpDatabase(db)
		if err != nil {
			return nil, fmt.Errorf("failed to dump %s: %w", db, err)
		}

		// Use sanitized filename
		filename := strings.ReplaceAll(db, "-", "_") + ".sql"
		dumps[filename] = dump
	}

	return dumps, nil
}

// CreateEnhancedBackup takes the original backup zip data and adds pg_dump files
func CreateEnhancedBackup(originalZip []byte, pgDumps map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)

	// First, copy all files from the original zip
	reader, err := zip.NewReader(bytes.NewReader(originalZip), int64(len(originalZip)))
	if err != nil {
		return nil, fmt.Errorf("failed to read original zip: %w", err)
	}

	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open %s: %w", file.Name, err)
		}

		writer, err := zipWriter.CreateHeader(&file.FileHeader)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("failed to create %s: %w", file.Name, err)
		}

		if _, err := io.Copy(writer, rc); err != nil {
			rc.Close()
			return nil, fmt.Errorf("failed to copy %s: %w", file.Name, err)
		}
		rc.Close()
	}

	// Add the pg_dump files
	for filename, data := range pgDumps {
		writer, err := zipWriter.Create("postgres/" + filename)
		if err != nil {
			return nil, fmt.Errorf("failed to create %s: %w", filename, err)
		}

		if _, err := writer.Write(data); err != nil {
			return nil, fmt.Errorf("failed to write %s: %w", filename, err)
		}
	}

	if err := zipWriter.Close(); err != nil {
		return nil, fmt.Errorf("failed to close zip: %w", err)
	}

	return buf.Bytes(), nil
}

// ExtractPostgresDumpsFromZip extracts postgres/*.sql files from a backup zip
func ExtractPostgresDumpsFromZip(zipData []byte) (map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("failed to open zip: %w", err)
	}

	dumps := make(map[string][]byte)
	for _, file := range reader.File {
		if strings.HasPrefix(file.Name, "postgres/") && strings.HasSuffix(file.Name, ".sql") {
			rc, err := file.Open()
			if err != nil {
				return nil, fmt.Errorf("failed to open %s: %w", file.Name, err)
			}

			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("failed to read %s: %w", file.Name, err)
			}

			// Extract just the filename without the postgres/ prefix
			filename := strings.TrimPrefix(file.Name, "postgres/")
			dumps[filename] = data
		}
	}

	return dumps, nil
}

// RestoreDatabase restores a database from a SQL dump using psql
// It first drops all existing objects to ensure a clean restore
func (c *PostgresConfig) RestoreDatabase(dbName string, sqlDump []byte) error {
	if dbName == "" {
		return fmt.Errorf("database name is empty")
	}

	// Filter out incompatible SET statements that may come from newer PostgreSQL versions
	// e.g., transaction_timeout is only available in PostgreSQL 17+
	filteredDump := filterIncompatibleStatements(sqlDump)

	// First, drop all objects in the database to ensure a clean restore
	// This drops all tables, views, functions, etc. in the public schema
	dropSQL := `
DO $$ DECLARE
    r RECORD;
BEGIN
    -- Drop all tables
    FOR r IN (SELECT tablename FROM pg_tables WHERE schemaname = 'public') LOOP
        EXECUTE 'DROP TABLE IF EXISTS "' || r.tablename || '" CASCADE';
    END LOOP;
    -- Drop all sequences
    FOR r IN (SELECT sequencename FROM pg_sequences WHERE schemaname = 'public') LOOP
        EXECUTE 'DROP SEQUENCE IF EXISTS "' || r.sequencename || '" CASCADE';
    END LOOP;
    -- Drop all views
    FOR r IN (SELECT viewname FROM pg_views WHERE schemaname = 'public') LOOP
        EXECUTE 'DROP VIEW IF EXISTS "' || r.viewname || '" CASCADE';
    END LOOP;
END $$;
`

	// Build connection args for psql
	baseArgs := []string{
		"-h", c.Host,
		"-p", c.Port,
		"-U", c.User,
		"-d", dbName,
		"--no-password", // We'll use PGPASSWORD env var
	}

	// First run the drop script
	dropCmd := exec.Command("psql", append(baseArgs, "-c", dropSQL)...)
	dropCmd.Env = append(dropCmd.Environ(), fmt.Sprintf("PGPASSWORD=%s", c.Password))
	var dropStderr bytes.Buffer
	dropCmd.Stderr = &dropStderr

	if err := dropCmd.Run(); err != nil {
		return fmt.Errorf("failed to drop existing objects: %w - %s", err, dropStderr.String())
	}

	// Now run the restore
	restoreArgs := append(baseArgs, "-v", "ON_ERROR_STOP=1")
	restoreCmd := exec.Command("psql", restoreArgs...)
	restoreCmd.Env = append(restoreCmd.Environ(), fmt.Sprintf("PGPASSWORD=%s", c.Password))
	restoreCmd.Stdin = bytes.NewReader(filteredDump)

	var stderr bytes.Buffer
	restoreCmd.Stderr = &stderr

	if err := restoreCmd.Run(); err != nil {
		return fmt.Errorf("psql restore failed: %w - %s", err, stderr.String())
	}

	return nil
}

// filterIncompatibleStatements removes SET statements for parameters
// that may not exist on older PostgreSQL versions
func filterIncompatibleStatements(sqlDump []byte) []byte {
	lines := strings.Split(string(sqlDump), "\n")
	var filtered []string

	// Parameters that are version-specific and may cause errors
	incompatibleParams := []string{
		"transaction_timeout", // PostgreSQL 17+
	}

	for _, line := range lines {
		skip := false
		trimmed := strings.TrimSpace(line)

		// Check if this is a SET statement for an incompatible parameter
		if strings.HasPrefix(strings.ToUpper(trimmed), "SET ") {
			for _, param := range incompatibleParams {
				if strings.Contains(strings.ToLower(trimmed), param) {
					skip = true
					break
				}
			}
		}

		if !skip {
			filtered = append(filtered, line)
		}
	}

	return []byte(strings.Join(filtered, "\n"))
}

// RestoreAllDatabases restores databases from SQL dump files
// The dumps map should have filenames like "main_db.sql" -> sql data
func (c *PostgresConfig) RestoreAllDatabases(dumps map[string][]byte) error {
	// Map sanitized filenames back to database names
	dbMap := map[string]string{}
	if c.MainDB != "" {
		sanitized := strings.ReplaceAll(c.MainDB, "-", "_") + ".sql"
		dbMap[sanitized] = c.MainDB
	}
	if c.LogDB != "" {
		sanitized := strings.ReplaceAll(c.LogDB, "-", "_") + ".sql"
		dbMap[sanitized] = c.LogDB
	}

	for filename, data := range dumps {
		dbName, ok := dbMap[filename]
		if !ok {
			// Try to infer database name from filename
			dbName = strings.TrimSuffix(filename, ".sql")
			dbName = strings.ReplaceAll(dbName, "_", "-")
		}

		if err := c.RestoreDatabase(dbName, data); err != nil {
			return fmt.Errorf("failed to restore %s: %w", dbName, err)
		}
	}

	return nil
}
