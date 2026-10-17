package render

import (
	"fmt"
	"sort"
	"strings"
)

// phpLiteral renders v as a PHP literal. Supported: string, bool, int,
// []any, map[string]any (keys sorted for stable output).
func phpLiteral(v any, indent string) string {
	switch x := v.(type) {
	case string:
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(x) + "'"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprint(x)
	case []any:
		var b strings.Builder
		b.WriteString("[\n")
		for i, e := range x {
			fmt.Fprintf(&b, "%s\t%d => %s,\n", indent, i, phpLiteral(e, indent+"\t"))
		}
		b.WriteString(indent + "]")
		return b.String()
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("[\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "%s\t%s => %s,\n", indent, phpLiteral(k, ""), phpLiteral(x[k], indent+"\t"))
		}
		b.WriteString(indent + "]")
		return b.String()
	}
	panic(fmt.Sprintf("phpLiteral: unsupported %T", v))
}

// PHPConfig renders a Nextcloud config file (config.php or *.config.php).
func PHPConfig(values map[string]any) []byte {
	return []byte("<?php\n// Pre-seeded by `ncwin setup`; Nextcloud manages this file from here on.\n$CONFIG = " + phpLiteral(values, "") + ";\n")
}
