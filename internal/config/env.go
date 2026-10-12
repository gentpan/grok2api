package config

import (
	"os"
	"sort"
	"strings"
)

// envKeyIndex is the set of config keys known when the environment overlay is
// applied (the merged defaults + user file), indexed for env-name resolution.
type envKeyIndex struct {
	// leaves maps the underscore-joined path ("account_refresh_enabled") to
	// the dotted key ("account.refresh.enabled"). On a collision between two
	// dotted keys the lexicographically smaller one wins, deterministically.
	leaves map[string]string
	// tables maps the underscore-joined path of every table ("account_refresh")
	// to its dotted path ("account.refresh").
	tables map[string]string
	// tableOrder lists the joined table paths longest first.
	tableOrder []string
}

func newEnvKeyIndex(data map[string]any) *envKeyIndex {
	idx := &envKeyIndex{leaves: map[string]string{}, tables: map[string]string{}}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			dotted := k
			if prefix != "" {
				dotted = prefix + "." + k
			}
			joined := strings.ReplaceAll(dotted, ".", "_")
			if sub, ok := v.(map[string]any); ok && len(sub) > 0 {
				if cur, dup := idx.tables[joined]; !dup || dotted < cur {
					idx.tables[joined] = dotted
				}
				walk(dotted, sub)
				continue
			}
			if cur, dup := idx.leaves[joined]; !dup || dotted < cur {
				idx.leaves[joined] = dotted
			}
		}
	}
	walk("", data)
	idx.tableOrder = make([]string, 0, len(idx.tables))
	for j := range idx.tables {
		idx.tableOrder = append(idx.tableOrder, j)
	}
	sort.Slice(idx.tableOrder, func(i, j int) bool {
		if len(idx.tableOrder[i]) != len(idx.tableOrder[j]) {
			return len(idx.tableOrder[i]) > len(idx.tableOrder[j])
		}
		return idx.tableOrder[i] < idx.tableOrder[j]
	})
	return idx
}

// resolve maps a lower-cased env name suffix (without the GROK_ prefix) to a
// dotted config key. ok is false when the name cannot be mapped.
//
//  1. exact match against a known leaf key (account_refresh_enabled);
//  2. otherwise the longest known table that prefixes the name, with the
//     remainder (underscores preserved) as the new key inside that table
//     (proxy_clearance_new_thing -> proxy.clearance.new_thing);
//  3. otherwise the legacy rule: split at the FIRST underscore.
//
// A name that is exactly a known table (GROK_PROXY_CLEARANCE) is rejected
// rather than replacing the table with a string.
func (idx *envKeyIndex) resolve(name string) (dotted string, ok bool) {
	if d, hit := idx.leaves[name]; hit {
		return d, true
	}
	if _, hit := idx.tables[name]; hit {
		return "", false
	}
	for _, j := range idx.tableOrder {
		if strings.HasPrefix(name, j+"_") && len(name) > len(j)+1 {
			return idx.tables[j] + "." + name[len(j)+1:], true
		}
	}
	i := strings.IndexByte(name, '_')
	if i <= 0 || i == len(name)-1 {
		return "", false
	}
	return name[:i] + "." + name[i+1:], true
}

// applyEnv overlays GROK_* environment variables onto data (all values are
// strings; getters coerce) and returns dotted key -> env variable name for
// every key that was set.
func applyEnv(data map[string]any, prefix string) map[string]string {
	idx := newEnvKeyIndex(data)
	var names []string
	for _, env := range os.Environ() {
		eq := strings.IndexByte(env, '=')
		if eq < 0 || !strings.HasPrefix(env[:eq], prefix) {
			continue
		}
		names = append(names, env[:eq])
	}
	sort.Strings(names) // deterministic when two names resolve to one key
	overrides := map[string]string{}
	for _, name := range names {
		val, set := os.LookupEnv(name)
		if !set {
			continue
		}
		dotted, ok := idx.resolve(strings.ToLower(name[len(prefix):]))
		if !ok {
			continue
		}
		setNested(data, strings.Split(dotted, "."), val)
		overrides[dotted] = name
	}
	return overrides
}

// setNested stores val at the path inside data, creating tables (and
// replacing non-table values in the way) as needed.
func setNested(data map[string]any, path []string, val any) {
	node := data
	for _, p := range path[:len(path)-1] {
		next, _ := node[p].(map[string]any)
		if next == nil {
			next = map[string]any{}
			node[p] = next
		}
		node = next
	}
	node[path[len(path)-1]] = val
}
