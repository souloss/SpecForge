// Package factcache stores normalized Contract Graph snapshots in SQLite.
// The content addressed engine memo remains the fast whole-run cache; this
// layer is an optional graph cache for callers that need to inspect or reuse
// the normalized graph independently of rendered artifacts.
package factcache

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/specforge/specforge/internal/contract"
	_ "modernc.org/sqlite"
)

type Cache struct{ db *sql.DB }

func Open(path string) (*Cache, error) {
	if path == "" {
		return &Cache{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS contract_graph_cache (
        fingerprint TEXT PRIMARY KEY,
        engine_version TEXT NOT NULL,
        graph_json BLOB NOT NULL,
        created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
    )`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS operation_dependencies (
        fingerprint TEXT NOT NULL,
        engine_version TEXT NOT NULL,
        operation_key TEXT NOT NULL,
        file_path TEXT NOT NULL,
        file_hash TEXT NOT NULL,
        PRIMARY KEY (fingerprint, engine_version, operation_key, file_path)
    )`); err != nil {
		db.Close()
		return nil, err
	}
	return &Cache{db: db}, nil
}

func (c *Cache) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

func (c *Cache) Load(fingerprint, engineVersion string) (*contract.Graph, bool, error) {
	if c == nil || c.db == nil {
		return nil, false, nil
	}
	var data []byte
	if err := c.db.QueryRow(`SELECT graph_json FROM contract_graph_cache WHERE fingerprint=? AND engine_version=?`, fingerprint, engineVersion).Scan(&data); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	graph := contract.New()
	if err := graph.Unmarshal(data); err != nil {
		return nil, false, err
	}
	return graph, true, nil
}

func (c *Cache) Save(fingerprint, engineVersion string, graph *contract.Graph) error {
	return c.SaveWithRoot(fingerprint, engineVersion, graph, "")
}

// SaveWithRoot records dependencies using actual file content hashes when root is provided.
func (c *Cache) SaveWithRoot(fingerprint, engineVersion string, graph *contract.Graph, root string) error {
	if c == nil || c.db == nil || graph == nil {
		return nil
	}
	data, err := graph.MarshalStable()
	if err != nil {
		return err
	}
	_, err = c.db.Exec(`INSERT INTO contract_graph_cache(fingerprint, engine_version, graph_json) VALUES(?,?,?)
        ON CONFLICT(fingerprint) DO UPDATE SET engine_version=excluded.engine_version, graph_json=excluded.graph_json, created_at=CURRENT_TIMESTAMP`, fingerprint, engineVersion, data)
	if err != nil {
		return err
	}
	if _, err = c.db.Exec(`DELETE FROM operation_dependencies WHERE fingerprint=? AND engine_version=?`, fingerprint, engineVersion); err != nil {
		return err
	}
	for key, operation := range graph.Operations {
		for _, evidence := range operationEvidence(operation) {
			hash := evidence.hash
			if root != "" {
				if fileHash, ok := contentHash(filepath.Join(root, filepath.FromSlash(evidence.file))); ok {
					hash = fileHash
				}
			}
			if _, err = c.db.Exec(`INSERT OR REPLACE INTO operation_dependencies(fingerprint, engine_version, operation_key, file_path, file_hash) VALUES(?,?,?,?,?)`, fingerprint, engineVersion, key, evidence.file, hash); err != nil {
				return err
			}
		}
	}
	return nil
}

func contentHash(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return fmt.Sprintf("%x", h.Sum(nil)), true
}

type dependency struct{ file, hash string }

func operationEvidence(operation *contract.Operation) []dependency {
	if operation == nil {
		return nil
	}
	var out []dependency
	add := func(items []contract.Evidence) {
		for _, item := range items {
			if item.Location.File == "" {
				continue
			}
			out = append(out, dependency{file: item.Location.File, hash: EvidenceHash(item)})
		}
	}
	add(operation.Evidence)
	for _, candidates := range operation.Parameters {
		for _, candidate := range candidates {
			add(candidate.Evidence)
		}
	}
	for _, candidate := range operation.RequestBody {
		add(candidate.Evidence)
	}
	for _, candidates := range operation.Responses {
		for _, candidate := range candidates {
			add(candidate.Evidence)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].hash < out[j].hash
	})
	return out
}

// EvidenceHash provides a stable dependency token for Contract Graph evidence.
func EvidenceHash(e contract.Evidence) string {
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}

// StaleOperations returns operations whose recorded evidence hash differs from current hashes.
func (c *Cache) StaleOperations(fingerprint, engineVersion string, current map[string]string) ([]string, error) {
	if c == nil || c.db == nil {
		return nil, nil
	}
	rows, err := c.db.Query(`SELECT DISTINCT operation_key, file_path, file_hash FROM operation_dependencies WHERE fingerprint=? AND engine_version=?`, fingerprint, engineVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stale := map[string]bool{}
	for rows.Next() {
		var key, path, hash string
		if err := rows.Scan(&key, &path, &hash); err != nil {
			return nil, err
		}
		if value, ok := current[path]; !ok || value != hash {
			stale[key] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(stale))
	for key := range stale {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}
