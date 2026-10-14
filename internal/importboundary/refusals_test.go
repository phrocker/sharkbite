// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package importboundary

import "testing"

func TestRefusedForms(t *testing.T) {
	for src, refused := range map[string]bool{
		"int f(void);\n":                           false,
		"#include \"a.h\"\r\nint x;\r\n":           false, // CRLF is normalised
		"printf(\"ERROR\");\n":                     false, // R" inside a word
		"asm(\"nop\");\n":                          true,
		"__asm (\"nop\");\n":                       true,
		"__asm__ volatile(\"nop\");\n":             true,
		"const char *s = \".incbin \\\"x\\\"\";\n": true,
		"/* .INCLUDE x */\n":                       true,
		"x = R\"(a)\";\n":                          true,
		"x = LR\"(a)\";\n":                         true,
		"x = u8R\"(a)\";\n":                        true,
		"x = UR\"(a)\";\n":                         true,
		"#embed \"blob\"\n":                        true,
		"#  embed <blob>\n":                        true,
		"#if __has_include(\"x.h\")\n#endif\n":     true,
		"#if __has_include_next(<x.h>)\n#endif\n":  true,
		"// hi\r#include \"x.h\"\n":                true,
		"int a;\rint b;\n":                         true,
		// gcc splices backslash-newline before tokenizing, with or without
		// spaces or tabs before the newline.
		"__a\\\nsm__(\".inc\\\nlude \\\"x\\\"\");\n": true,
		"x = R\\\n\"x(\" /* )x\";\n":                 true,
		"__as\\ \t\nm__(\"nop\");\n":                 true,
		"int ok\\\n_value;\n":                        false,
	} {
		if got := len(refusals([]byte(src))) > 0; got != refused {
			t.Errorf("%q: refused=%v, want %v", src, got, refused)
		}
	}
}

func TestExtensionFileTypes(t *testing.T) {
	for name, ok := range map[string]bool{
		"extensions/e/main.go":                  true,
		"extensions/e/go.mod":                   true,
		"extensions/e/go.sum":                   true,
		"extensions/e/README.md":                true,
		"extensions/e/testdata/case.json":       true,
		"extensions/e/testdata/sub/case.golden": true,
		"extensions/e/notes.txt":                false,
		"extensions/e/config.yaml":              false,
		"extensions/e/data.json":                false, // only under testdata
		"extensions/e/LICENSE":                  false,
		"extensions/e/.gitignore":               false,
		"extensions/e/testdata/payload.txt":     false,
		"extensions/e/testdata/x.s":             false,
		"extensions/e/leak.h":                   false,
		"extensions/e/testdata.json":            false,
	} {
		if extensionFileAllowed(name) != ok {
			t.Errorf("%s: allowed=%v, want %v", name, !ok, ok)
		}
	}
}
