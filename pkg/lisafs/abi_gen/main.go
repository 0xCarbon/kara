// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// abi_gen regenerates the MID table in pkg/lisafs/ABI.md from the MID const
// block in message.go, so that the documented table can never drift from the
// wire definitions. Run it from pkg/lisafs with:
//
//	go generate ./...
//
// The table is emitted between the BEGIN/END GENERATED markers in ABI.md;
// everything outside those markers is preserved. abi_conformance_test.go
// verifies that the committed ABI.md matches what this tool would emit, so
// editing message.go's MID block without regenerating ABI.md fails CI.
//
// The tool parses message.go with go/ast and does not depend on the rest of
// the package, so it works on the master tree (bazel-only, generated code
// absent) as well as on the go branch.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
)

var (
	srcPath = flag.String("src", "message.go", "path to message.go")
	docPath = flag.String("doc", "ABI.md", "path to ABI.md")
)

const (
	beginMarker = "<!-- BEGIN GENERATED MID TABLE (go:generate go run ./abi_gen) -->"
	endMarker   = "<!-- END GENERATED MID TABLE -->"
)

// midEntry is one row of the generated table.
type midEntry struct {
	value uint16
	name  string
	doc   string
}

// extractMIDs parses the MID const block from message.go. It returns one
// entry per MID constant, ordered by value.
func extractMIDs(src []byte) ([]midEntry, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, *srcPath, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var entries []midEntry
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			// The MID block's first spec is `Error MID = 0`; only specs whose
			// type is the MID type belong to the table.
			if vt, ok := vs.Type.(*ast.Ident); !ok || vt.Name != "MID" {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				return nil, fmt.Errorf("MID %s: value is not an integer literal", vs.Names[0].Name)
			}
			v, err := strconv.ParseUint(lit.Value, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("MID %s: %v", vs.Names[0].Name, err)
			}
			doc := ""
			if vs.Doc != nil {
				doc = strings.Join(strings.Fields(vs.Doc.Text()), " ")
			}
			entries = append(entries, midEntry{value: uint16(v), name: vs.Names[0].Name, doc: doc})
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no MID constants found in %s", *srcPath)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].value < entries[j].value })
	for i, e := range entries {
		if e.value != uint16(i) {
			return nil, fmt.Errorf("MID values must be contiguous from 0; got %s = %d after %d entries", e.name, e.value, i)
		}
	}
	return entries, nil
}

// renderTable emits the markdown table for the given entries.
func renderTable(entries []midEntry) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "| MID | Name    | Purpose |\n")
	fmt.Fprintf(&buf, "|----:|---------|---------|\n")
	for _, e := range entries {
		fmt.Fprintf(&buf, "| %3d | `%s`", e.value, e.name)
		for pad := len(e.name); pad < 7; pad++ {
			buf.WriteString(" ")
		}
		fmt.Fprintf(&buf, "| %s |\n", e.doc)
	}
	return buf.String()
}

func main() {
	flag.Parse()

	src, err := os.ReadFile(*srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abi_gen: %v\n", err)
		os.Exit(1)
	}
	entries, err := extractMIDs(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abi_gen: %v\n", err)
		os.Exit(1)
	}

	doc, err := os.ReadFile(*docPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abi_gen: %v\n", err)
		os.Exit(1)
	}
	begin := bytes.Index(doc, []byte(beginMarker))
	end := bytes.Index(doc, []byte(endMarker))
	if begin < 0 || end < 0 || end < begin {
		fmt.Fprintf(os.Stderr, "abi_gen: markers not found in %s\n", *docPath)
		os.Exit(1)
	}

	var out bytes.Buffer
	out.Write(doc[:begin])
	out.WriteString(beginMarker + "\n\n")
	out.WriteString(renderTable(entries))
	out.WriteString("\n" + endMarker)
	rest := doc[end+len(endMarker):]
	out.Write(rest)

	if err := os.WriteFile(*docPath, out.Bytes(), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "abi_gen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("abi_gen: wrote %d MID entries to %s\n", len(entries), *docPath)
}
