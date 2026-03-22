// state/reader.go — HisnOS state file reader
//
// Parses the key=value state files written by hisnos-update.sh and
// hisnos-vault.sh telemetry. Format:
//
//   key=value
//   # comment lines are ignored
//   # empty lines are ignored
//   key2=value with = signs in it   ← Cut on first = only

package state

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// ReadFile reads a key=value state file from path and returns the parsed map.
// Returns an empty map (not an error) if the file does not exist.
func ReadFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseKVReader(f), nil
}

// ParseKV parses a key=value multi-line string and returns the parsed map.
// Later duplicate keys overwrite earlier ones.
func ParseKV(s string) map[string]string {
	return ParseKVReader(strings.NewReader(s))
}

// ParseKVReader parses key=value lines from r.
func ParseKVReader(r io.Reader) map[string]string {
	m := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}
