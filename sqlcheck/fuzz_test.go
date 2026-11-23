package sqlcheck

import (
	"testing"
)

func FuzzParseSnapshot(f *testing.F) {
	f.Add("-- header\n\n-- public.items\n-- get\nSELECT id FROM public.items WHERE id = $1;\n")
	f.Add("\n-- public.a\n-- insert\nINSERT INTO public.a (id)\nVALUES ($1);\n")
	f.Add("")
	f.Add("-- only comment")
	f.Add("SELECT 1;")
	f.Add("-- t\n-- k\n;\n")
	f.Fuzz(func(t *testing.T, raw string) {
		defs, err := ParseSnapshot(raw)
		if err != nil || len(defs) == 0 {
			return
		}
		// Any valid parsed snapshot must round-trip cleanly through RenderSnapshot -> ParseSnapshot.
		rendered := RenderSnapshot(defs)
		reparsed, err := ParseSnapshot(rendered)
		if err != nil {
			t.Fatalf("round-trip ParseSnapshot failed: %v\nrendered:\n%s", err, rendered)
		}
		if len(reparsed) != len(defs) {
			t.Fatalf("round-trip table count mismatch: %d vs %d", len(reparsed), len(defs))
		}
	})
}
