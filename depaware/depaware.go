// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package depaware is the guts of the depaware program.
//
// It's in its own package so others can empty-import depend on it and
// pin a specific version of depaware in their go.mod files.
package depaware

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/pkg/diff"
	"github.com/pkg/diff/write"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/imports"
)

var (
	check    = flag.Bool("check", false, "if true, check whether dependencies match the depaware.txt file")
	update   = flag.Bool("update", false, "if true, update the depaware.txt file")
	fileName = flag.String("file", "depaware.txt", "name of the file to write")
	osList   = flag.String("goos", "linux,darwin,windows", "comma-separated list of GOOS values")
	tags     = flag.String("tags", "", "comma-separated list of build tags to use when loading packages")
	internal = flag.Bool("internal", false, "if true, include internal packages in the output")
)

func Main() {
	flag.Parse()
	if *check && *update {
		log.Fatalf("-check and -update can't be used together")
	}

	ipaths, err := pkgPaths(flag.Args()...)
	if err != nil {
		log.Fatalf("could not resolve packages: %v", err)
	}
	for _, pkg := range ipaths {
		if strings.HasPrefix(pkg, "-") {
			log.Fatalf("bogus package argument %q; flags go before packages", pkg)
		}
	}
	for i, pkg := range ipaths {
		process(pkg)
		// If we're printing to stdout, and there are more packages to come,
		// add an extra newline.
		if i != len(ipaths)-1 && !*check && !*update {
			fmt.Println()
		}
	}
}

func process(pkg string) {
	geese := strings.Split(*osList, ",")
	var d deps
	var dir string
	var buildFlags []string
	if *tags != "" {
		buildFlags = append(buildFlags, "-tags", *tags)
	}
	for _, goos := range geese {
		env := os.Environ()
		env = append(env, "GOARCH=amd64", "GOOS="+goos, "CGO_ENABLED=1")
		cfg := &packages.Config{
			Mode:       packages.NeedImports | packages.NeedDeps | packages.NeedFiles | packages.NeedName | packages.NeedCompiledGoFiles,
			Env:        env,
			BuildFlags: buildFlags,
		}

		pkgs, err := packages.Load(cfg, pkg)
		if err != nil {
			log.Fatalf("for GOOS=%v: %v", goos, err)
		}

		packages.Visit(pkgs, nil, func(p *packages.Package) {
			for imp := range p.Imports {
				d.AddEdge(p.PkgPath, imp)
			}
			if p.PkgPath == pkg {
				if dir == "" && len(p.GoFiles) > 0 {
					dir = filepath.Dir(p.GoFiles[0])
				}
				return
			}
			d.AddDep(p.PkgPath, goos)
		})
	}

	if dir == "" {
		log.Fatalf("no .go files found for package %s", pkg)
	}

	sort.Slice(d.Deps, func(i, j int) bool {
		d1, d2 := d.Deps[i], d.Deps[j]
		if p1, p2 := strings.Contains(d1, "."), strings.Contains(d2, "."); p1 != p2 {
			return p1
		}
		if x1, x2 := strings.Contains(d1, "golang.org/x/"), strings.Contains(d2, "golang.org/x/"); x1 != x2 {
			return x2
		}
		return d1 < d2
	})

	// Parse existing depaware.txt, if present,
	// to get the existing dependency source the file lists.
	daFile := filepath.Join(dir, *fileName)
	daContents, daErr := ioutil.ReadFile(daFile)
	var preferredWhy map[string]string
	if daErr == nil {
		preferredWhy = parsePreferredWhy(bytes.NewReader(daContents))
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s dependencies: (generated by github.com/tailscale/depaware)\n\n", pkg)
	var osBuf bytes.Buffer

	for _, pkg := range d.Deps {
		unsafeIcon := " "
		cgoIcon := " "
		if d.UsesUnsafe[pkg] && !isGoPackage(pkg) {
			unsafeIcon = "U"
		}
		if d.UsesCGO[pkg] && !isGoPackage(pkg) {
			cgoIcon = "C"
		}
		osBuf.Reset()
		for _, goos := range geese {
			if d.DepOnOS[pkgGOOS{pkg, goos}] {
				osBuf.WriteRune(unicode.ToUpper(rune(goos[0])))
			}
		}
		if osBuf.Len() == len(geese) {
			osBuf.Reset()
		}
		fmt.Fprintf(&buf, " %3s %s%s %-60s %s\n", osBuf.Bytes(), unsafeIcon, cgoIcon, pkg, d.Why(pkg, preferredWhy))
	}

	if *check {
		if daErr != nil {
			log.Fatal(daErr)
		}
		if bytes.Equal(daContents, buf.Bytes()) {
			// Success. No changes.
			return
		}
		var opts []write.Option
		const wantColor = false // https://github.com/tailscale/depaware/issues/11
		if wantColor && os.Getenv("TERM") != "dumb" {
			opts = append(opts, write.TerminalColor())
		}
		fmt.Fprintf(os.Stderr, "The list of dependencies in %s is out of date.\n\n", daFile)
		err := diff.Text("before", "after", daContents, buf.Bytes(), os.Stderr, opts...)
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(1)
	}

	if *update {
		if err := ioutil.WriteFile(daFile, buf.Bytes(), 0644); err != nil {
			log.Fatal(err)
		}
		return
	}

	os.Stdout.Write(buf.Bytes())
}

type pkgGOOS struct {
	pkg  string
	goos string
}

type deps struct {
	Deps    []string
	DepOnOS map[pkgGOOS]bool // {pkg, goos} -> true

	DepTo      map[string][]string // pkg in key is imported by packages in value
	UsesUnsafe map[string]bool
	UsesCGO    map[string]bool
}

func (d *deps) Why(pkg string, preferredWhy map[string]string) string {
	from := d.DepTo[pkg]
	if len(from) == 0 {
		return ""
	}
	var why string
	pref := preferredWhy[pkg]
	// Check whether the preferred "why" package is in from.
	if pref != "" {
		for _, f := range from {
			if pref == f {
				why = pref
				break
			}
		}
	}
	// If it's not select the lexigraphically first package in from.
	if why == "" {
		sort.Strings(from)
		why = from[0]
	}
	plus := ""
	if len(from) > 1 {
		plus = "+"
	}
	return "from " + why + plus
}

func (d *deps) AddEdge(from, to string) {
	from = imports.VendorlessPath(from)
	to = imports.VendorlessPath(to)
	if d.DepTo == nil {
		d.DepTo = make(map[string][]string)
		d.UsesUnsafe = make(map[string]bool)
		d.UsesCGO = make(map[string]bool)
	}
	if !stringsContains(d.DepTo[to], from) {
		d.DepTo[to] = append(d.DepTo[to], from)
	}
	if to == "unsafe" {
		d.UsesUnsafe[from] = true
	}
	if to == "runtime/cgo" {
		d.UsesCGO[from] = true
	}
}

func (d *deps) AddDep(pkg, goos string) {
	pkg = imports.VendorlessPath(pkg)
	if !*internal && isInternalPackage(pkg) {
		return
	}
	if !stringsContains(d.Deps, pkg) {
		d.Deps = append(d.Deps, pkg)
	}
	if d.DepOnOS == nil {
		d.DepOnOS = map[pkgGOOS]bool{}
	}
	d.DepOnOS[pkgGOOS{pkg, goos}] = true
}

func stringsContains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func isInternalPackage(pkg string) bool {
	return strings.HasPrefix(pkg, "internal/") ||
		strings.HasPrefix(pkg, "runtime/internal/") ||
		pkg == "runtime" || pkg == "runtime/cgo" || pkg == "unsafe" ||
		(strings.Contains(pkg, "/internal/") && isGoPackage(pkg))
}

func isGoPackage(pkg string) bool {
	return !strings.Contains(pkg, ".") ||
		strings.Contains(pkg, "golang.org/x")
}

// pkgPaths resolves pkg to a slice of Go package import paths.
// See https://golang.org/issue/30826 and https://golang.org/issue/30828.
func pkgPaths(pkg ...string) (ipaths []string, err error) {
	pkgs, err := packages.Load(nil, pkg...)
	if err != nil {
		return nil, err
	}
	for _, p := range pkgs {
		ipaths = append(ipaths, p.PkgPath)
	}
	return ipaths, nil
}

// parsePreferredWhy parses an existing depaware.txt.
// It returns a preferred source for each dependency.
// For example, given:
//
// [...]
// encoding           from encoding/json
// encoding/binary    from encoding/base64+
//
// It returns {"encoding": "encoding/json", "encoding/binary": "encoding/base64"}.
// The goal is to minimize diffs when introducing a new, lexicographically prior dependency source.
//
// parsePreferredWhy is best effort only.
func parsePreferredWhy(r io.Reader) map[string]string {
	m := make(map[string]string)
	scan := bufio.NewScanner(r)
	for scan.Scan() {
		words := bytes.Fields(scan.Bytes())
		// look for the word "from". The preceding and succeeding words are the dependency and its source.
		from := []byte("from")
		var i int
		for i = range words {
			if bytes.Equal(words[i], from) {
				break
			}
		}
		if i < 1 || i >= len(words)-1 {
			continue
		}
		dep := words[i-1]
		src := words[i+1]
		src = bytes.TrimRight(src, "+")
		m[string(dep)] = string(src)
	}
	return m
}
