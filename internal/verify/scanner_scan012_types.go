package verify

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

const scan012LoadTimeout = 90 * time.Second

type scan012PackageFiles struct {
	dir   string
	files []string
}

// scanInterfaceDefaultImpl keeps the direct helper useful for package-level
// tests. The production scanner batches all internal packages in one load via
// scanInterfaceDefaultImplPackages so cross-package implementations share the
// same go/types package graph.
func scanInterfaceDefaultImpl(dir string, goFiles []string) []Finding {
	return scanInterfaceDefaultImplPackages(internalProductionRoot(dir), []scan012PackageFiles{{dir: dir, files: goFiles}})
}

// scanInterfaceDefaultImplPackages compares runtime interfaces declared by
// core packages with concrete types from all non-test internal production
// packages. One go/packages load gives types.Implements the package identity
// information required to recognize cross-package method sets and promotions.
func scanInterfaceDefaultImplPackages(internalRoot string, interfacePackages []scan012PackageFiles) []Finding {
	targetFiles := make(map[string][]string, len(interfacePackages))
	for _, target := range interfacePackages {
		hasProductionFile := false
		for _, file := range target.files {
			if !strings.HasSuffix(file, "_test.go") {
				hasProductionFile = true
				break
			}
		}
		if !hasProductionFile {
			continue
		}
		absDir, err := filepath.Abs(target.dir)
		if err != nil {
			return scan012LoadFailure(target.dir, "resolve interface package directory: "+err.Error())
		}
		targetFiles[filepath.Clean(absDir)] = target.files
	}
	if len(targetFiles) == 0 {
		return nil
	}

	absRoot, err := filepath.Abs(internalRoot)
	if err != nil {
		return scan012LoadFailure(internalRoot, "resolve internal package root: "+err.Error())
	}
	absRoot = filepath.Clean(absRoot)
	ctx, cancel := context.WithTimeout(context.Background(), scan012LoadTimeout)
	defer cancel()

	mode := packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
		packages.NeedSyntax | packages.NeedTypes
	loaded, err := packages.Load(&packages.Config{
		Context: ctx,
		Dir:     absRoot,
		Mode:    mode,
		Tests:   false,
	}, "./...")
	if err != nil {
		return scan012LoadFailure(absRoot, "load internal production packages: "+err.Error())
	}
	if len(loaded) == 0 {
		return scan012LoadFailure(absRoot, "no internal production packages were loaded")
	}

	packagesByDir := make(map[string]*packages.Package, len(loaded))
	for _, pkg := range loaded {
		pkgDir, absErr := filepath.Abs(pkg.Dir)
		if absErr != nil {
			return scan012LoadFailure(pkg.Dir, "resolve loaded package directory: "+absErr.Error())
		}
		pkgDir = filepath.Clean(pkgDir)
		if !isWithinDir(absRoot, pkgDir) || isVerifyPackageDir(absRoot, pkgDir) {
			continue
		}
		packagesByDir[pkgDir] = pkg
	}

	var loadFindings []Finding
	var implementations []types.Type
	packageDirs := make([]string, 0, len(packagesByDir))
	for pkgDir := range packagesByDir {
		packageDirs = append(packageDirs, pkgDir)
	}
	sort.Strings(packageDirs)
	for _, pkgDir := range packageDirs {
		pkg := packagesByDir[pkgDir]
		if len(pkg.Errors) > 0 {
			for _, packageErr := range pkg.Errors {
				fallback := pkgDir
				if len(pkg.GoFiles) > 0 {
					fallback = pkg.GoFiles[0]
				}
				file, line := scan012ErrorPosition(packageErr.Pos, fallback, pkg.GoFiles)
				loadFindings = append(loadFindings, Finding{
					RuleID:   "SCAN-012",
					Severity: SeverityError,
					File:     file,
					Line:     line,
					Message:  "could not type-check SCAN-012 package " + pkg.PkgPath + ": " + packageErr.Msg,
				})
			}
			continue
		}
		if pkg.IllTyped || pkg.Types == nil {
			loadFindings = append(loadFindings, scan012LoadFailure(pkgDir, "type checker returned incomplete information for package "+pkg.PkgPath)...)
			continue
		}
		implementations = append(implementations, concretePackageTypes(pkg.Types)...)
	}
	if len(loadFindings) > 0 {
		return deduplicateScan012Findings(loadFindings)
	}

	var targets []*packages.Package
	for pkgDir := range targetFiles {
		pkg, ok := packagesByDir[pkgDir]
		if !ok {
			return scan012LoadFailure(pkgDir, "core interface package was not included in the internal package load")
		}
		targets = append(targets, pkg)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].PkgPath < targets[j].PkgPath })

	var findings []Finding
	for _, pkg := range targets {
		goFiles := targetFiles[filepath.Clean(pkg.Dir)]
		for _, file := range pkg.Syntax {
			for _, declaration := range file.Decls {
				general, ok := declaration.(*ast.GenDecl)
				if !ok || general.Tok != token.TYPE {
					continue
				}
				for _, spec := range general.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					object, ok := pkg.Types.Scope().Lookup(typeSpec.Name.Name).(*types.TypeName)
					if !ok {
						position := pkg.Fset.Position(typeSpec.Pos())
						findings = append(findings, Finding{
							RuleID:   "SCAN-012",
							Severity: SeverityError,
							File:     scan012SourcePath(position.Filename, goFiles),
							Line:     position.Line,
							Message:  "could not resolve core type " + typeSpec.Name.Name + " in SCAN-012 type information",
							Snippet:  typeSpec.Name.Name,
						})
						continue
					}
					if object.IsAlias() {
						continue
					}
					named, ok := object.Type().(*types.Named)
					if !ok {
						position := pkg.Fset.Position(typeSpec.Pos())
						findings = append(findings, Finding{
							RuleID:   "SCAN-012",
							Severity: SeverityError,
							File:     scan012SourcePath(position.Filename, goFiles),
							Line:     position.Line,
							Message:  "core type " + object.Name() + " is missing its named-type information",
							Snippet:  object.Name(),
						})
						continue
					}
					iface, ok := named.Underlying().(*types.Interface)
					if !ok {
						continue
					}
					iface.Complete()
					// Only non-basic interfaces with type terms are constraints, not
					// runtime interface values. A method-set interface with type
					// parameters is still instantiable; without a concrete instance we
					// report unresolved evidence instead of silently excluding it.
					if !iface.IsMethodSet() || iface.NumMethods() == 0 {
						continue
					}
					position := pkg.Fset.Position(typeSpec.Pos())
					if named.TypeParams() != nil && named.TypeParams().Len() > 0 {
						findings = append(findings, Finding{
							RuleID:   "SCAN-012",
							Severity: SeverityError,
							File:     scan012SourcePath(position.Filename, goFiles),
							Line:     position.Line,
							Message:  "cannot prove a default implementation for generic runtime interface " + object.Name() + " without an instantiated type",
							Snippet:  object.Name(),
						})
						continue
					}
					if hasConcreteImplementation(implementations, iface) {
						continue
					}

					findings = append(findings, Finding{
						RuleID:   "SCAN-012",
						Severity: SeverityError,
						File:     scan012SourcePath(position.Filename, goFiles),
						Line:     position.Line,
						Message:  "interface " + object.Name() + " has no proven internal concrete implementation among non-generic types",
						Snippet:  object.Name(),
					})
				}
			}
		}
	}
	return findings
}

func internalProductionRoot(dir string) string {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	absDir = filepath.Clean(absDir)
	if filepath.Base(absDir) == "internal" {
		return absDir
	}
	parent := filepath.Dir(absDir)
	if filepath.Base(parent) == "internal" {
		return parent
	}
	child := filepath.Join(absDir, "internal")
	if info, statErr := os.Stat(child); statErr == nil && info.IsDir() {
		return child
	}
	return absDir
}

func isWithinDir(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func isVerifyPackageDir(internalRoot, packageDir string) bool {
	relative, err := filepath.Rel(internalRoot, packageDir)
	if err != nil {
		return false
	}
	relative = filepath.ToSlash(relative)
	return relative == "verify" || strings.HasPrefix(relative, "verify/")
}

func deduplicateScan012Findings(findings []Finding) []Finding {
	seen := make(map[string]struct{}, len(findings))
	unique := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		key := fmt.Sprintf("%s\x00%d\x00%s", finding.File, finding.Line, finding.Message)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, finding)
	}
	return unique
}

func concretePackageTypes(pkg *types.Package) []types.Type {
	if pkg == nil {
		return nil
	}
	var candidates []types.Type
	for _, name := range pkg.Scope().Names() {
		typeName, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok || typeName.IsAlias() {
			continue
		}
		named, ok := typeName.Type().(*types.Named)
		if !ok || (named.TypeParams() != nil && named.TypeParams().Len() > 0) {
			continue
		}
		if _, isInterface := named.Underlying().(*types.Interface); isInterface {
			continue
		}
		candidates = append(candidates, named, types.NewPointer(named))
	}
	return candidates
}

func hasConcreteImplementation(candidates []types.Type, iface *types.Interface) bool {
	for _, candidate := range candidates {
		if types.Implements(candidate, iface) {
			return true
		}
	}
	return false
}

func scan012SourcePath(typedPath string, goFiles []string) string {
	if typedPath == "" {
		return typedPath
	}
	cleanTyped := filepath.Clean(typedPath)
	for _, sourcePath := range goFiles {
		absSource, err := filepath.Abs(sourcePath)
		if err == nil && filepath.Clean(absSource) == cleanTyped {
			return sourcePath
		}
	}
	return typedPath
}

func scan012ErrorPosition(rawPosition, fallback string, goFiles []string) (string, int) {
	if rawPosition == "" {
		return fallback, 0
	}

	withoutColumn := rawPosition
	if separator := strings.LastIndex(withoutColumn, ":"); separator >= 0 {
		if _, err := strconv.Atoi(withoutColumn[separator+1:]); err == nil {
			withoutColumn = withoutColumn[:separator]
		}
	}
	separator := strings.LastIndex(withoutColumn, ":")
	if separator < 0 {
		return scan012SourcePath(rawPosition, goFiles), 0
	}
	line, err := strconv.Atoi(withoutColumn[separator+1:])
	if err != nil {
		return scan012SourcePath(rawPosition, goFiles), 0
	}
	return scan012SourcePath(withoutColumn[:separator], goFiles), line
}

func scan012LoadFailure(dir, reason string) []Finding {
	return []Finding{{
		RuleID:   "SCAN-012",
		Severity: SeverityError,
		File:     dir,
		Message:  "could not establish SCAN-012 type information: " + reason,
	}}
}
