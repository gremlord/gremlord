package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ~/.gremlord/env holds KEY=VALUE lines (0600) so provider keys don't
// depend on which shell happened to launch the router leader. Process
// environment still wins when set.

var envFile struct {
	mu     sync.Mutex
	path   string
	mtime  time.Time
	values map[string]string
}

// EnvFileLookup returns the value for name from ~/.gremlord/env, or "".
func EnvFileLookup(name string) string {
	dir, err := DataDir()
	if err != nil {
		return ""
	}
	path := readPath(filepath.Join(dir, "env"))

	envFile.mu.Lock()
	defer envFile.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if envFile.path != path || !info.ModTime().Equal(envFile.mtime) {
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		values := map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
		envFile.path, envFile.mtime, envFile.values = path, info.ModTime(), values
	}
	return envFile.values[name]
}
