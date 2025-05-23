package reviser

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/walteh/goimports-reviser/v3/pkg/astutil"
	"github.com/walteh/goimports-reviser/v3/pkg/std"
)

const (
	StandardInput        = "<standard-input>"
	stringValueSeparator = ","
)

var (
	codeGeneratedPattern = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)
)

// SourceFile main struct for fixing an existing code
type SourceFile struct {
	shouldRemoveUnusedImports      bool
	shouldUseAliasForVersionSuffix bool
	shouldFormatCode               bool
	shouldSkipAutoGenerated        bool
	shouldSeparateNamedImports     bool
	hasSeparateSideEffectGroup     bool
	companyPackagePrefixes         []string
	importsOrders                  ImportsOrders
	renameImports                  map[string]string

	projectName string
	filePath    string
	reader      io.Reader
}

// NewSourceFile constructor
func NewSourceFile(projectName, filePath string) *SourceFile {
	return &SourceFile{
		projectName: projectName,
		filePath:    filePath,
	}
}

// Fix is for revise imports and format the code. Returns formated content, original content, true if formatted content is different from original and error.
func (f *SourceFile) Fix(options ...SourceFileOption) ([]byte, []byte, bool, error) {
	if f.filePath == StandardInput && f.reader == nil {
		options = append(options, WithReader(os.Stdin))
	}

	for _, option := range options {
		err := option(f)
		if err != nil {
			return nil, nil, false, err
		}
	}

	var originalContent []byte
	var err error
	if f.reader != nil {
		originalContent, err = io.ReadAll(f.reader)
	} else {
		originalContent, err = os.ReadFile(f.filePath)
	}
	if err != nil {
		return nil, originalContent, false, err
	}

	fset := token.NewFileSet()

	pf, err := parser.ParseFile(fset, "", originalContent, parser.ParseComments)
	if err != nil {
		return nil, originalContent, false, err
	}

	if f.shouldSkipAutoGenerated && isFileAutoGenerate(pf) {
		return originalContent, originalContent, false, nil
	}

	importsWithMetadata, err := f.parseImports(pf)
	if err != nil {
		return nil, originalContent, false, err
	}

	groups := f.groupImports(
		f.projectName,
		f.companyPackagePrefixes,
		importsWithMetadata,
	)

	decls, ok := hasMultipleImportDecls(pf)
	if ok {
		pf.Decls = decls
	}

	f.fixImports(pf, groups, importsWithMetadata)

	f.formatDecls(pf)

	fixedImportsContent, err := generateFile(fset, pf)
	if err != nil {
		return nil, originalContent, false, err
	}

	formattedContent, err := format.Source(fixedImportsContent)
	if err != nil {
		return nil, originalContent, false, err
	}

	return formattedContent, originalContent, !bytes.Equal(originalContent, formattedContent), nil
}

func isFileAutoGenerate(pf *ast.File) bool {
	for _, comment := range pf.Comments {
		for _, c := range comment.List {
			if codeGeneratedPattern.MatchString(c.Text) && c.Pos() < pf.Package {
				return true
			}
		}
	}
	return false
}

func (f *SourceFile) formatDecls(file *ast.File) {
	if !f.shouldFormatCode {
		return
	}

	for _, decl := range file.Decls {
		switch dd := decl.(type) {
		case *ast.GenDecl:
			dd.Doc = fixCommentGroup(dd.Doc)
		case *ast.FuncDecl:
			dd.Doc = fixCommentGroup(dd.Doc)
		}
	}
}

func fixCommentGroup(commentGroup *ast.CommentGroup) *ast.CommentGroup {
	if commentGroup == nil {
		formattedDoc := &ast.CommentGroup{
			List: []*ast.Comment{},
		}

		return formattedDoc
	}

	formattedDoc := &ast.CommentGroup{
		List: make([]*ast.Comment, len(commentGroup.List)),
	}

	copy(formattedDoc.List, commentGroup.List)

	return formattedDoc
}

func (f *SourceFile) groupImports(
	projectName string,
	localPkgPrefixes []string,
	importsWithMetadata map[string]*commentsMetadata,
) *groupsImports {
	var (
		stdImports            []string
		xImports              []string
		projectImports        []string
		projectLocalPkgs      []string
		generalImports        []string
		namedStdImports       []string
		namedXImports         []string
		namedProjectImports   []string
		namedProjectLocalPkgs []string
		namedGeneralImports   []string
		blankedImports        []string
		dottedImports         []string
	)

	for imprt := range importsWithMetadata {
		if f.importsOrders.hasBlankedImportOrder() && strings.HasPrefix(imprt, "_") {
			blankedImports = append(blankedImports, imprt)
			continue
		}

		if f.importsOrders.hasDottedImportOrder() && strings.HasPrefix(imprt, ".") {
			dottedImports = append(dottedImports, imprt)
			continue
		}

		pkgWithoutAlias := skipPackageAlias(imprt)
		values := strings.Split(imprt, " ")

		if strings.Contains(imprt, "\"golang.org/x/") || strings.Contains(imprt, "\"github.com/pkg/") {
			if f.shouldSeparateNamedImports {
				if len(values) > 1 {
					namedXImports = append(namedXImports, imprt)
				} else {
					xImports = append(xImports, imprt)
				}
				continue
			}
			xImports = append(xImports, imprt)
			continue
		}

		if _, ok := std.StdPackages[pkgWithoutAlias]; ok {
			if f.shouldSeparateNamedImports {
				if len(values) > 1 {
					namedStdImports = append(namedStdImports, imprt)
				} else {
					stdImports = append(stdImports, imprt)
				}
				continue
			}
			stdImports = append(stdImports, imprt)
			continue
		}

		var isLocalPackageFound bool
		for _, localPackagePrefix := range localPkgPrefixes {
			if strings.HasPrefix(pkgWithoutAlias, localPackagePrefix) && !strings.HasPrefix(pkgWithoutAlias, projectName) {
				if f.shouldSeparateNamedImports {
					if len(values) > 1 {
						namedProjectLocalPkgs = append(namedProjectLocalPkgs, imprt)
					} else {
						projectLocalPkgs = append(projectLocalPkgs, imprt)
					}
					isLocalPackageFound = true
					break
				}
				projectLocalPkgs = append(projectLocalPkgs, imprt)
				isLocalPackageFound = true
				break
			}
		}

		if isLocalPackageFound {
			continue
		}

		if strings.Contains(pkgWithoutAlias, projectName) {
			if f.shouldSeparateNamedImports {
				if len(values) > 1 {
					namedProjectImports = append(namedProjectImports, imprt)
				} else {
					projectImports = append(projectImports, imprt)
				}
				continue
			}
			projectImports = append(projectImports, imprt)
			continue
		}

		if f.shouldSeparateNamedImports {
			if len(values) > 1 {
				namedGeneralImports = append(namedGeneralImports, imprt)
			} else {
				generalImports = append(generalImports, imprt)
			}
			continue
		}
		generalImports = append(generalImports, imprt)
	}

	sort.Strings(stdImports)
	sort.Strings(xImports)
	sort.Strings(generalImports)
	sort.Strings(projectLocalPkgs)
	sort.Strings(projectImports)
	sort.Strings(blankedImports)
	sort.Strings(dottedImports)
	sort.Strings(namedStdImports)
	sort.Strings(namedXImports)
	sort.Strings(namedGeneralImports)
	sort.Strings(namedProjectLocalPkgs)
	sort.Strings(namedProjectImports)

	result := &groupsImports{
		common: &common{
			std:          stdImports,
			namedStd:     namedStdImports,
			x:            xImports,
			namedX:       namedXImports,
			general:      generalImports,
			namedGeneral: namedGeneralImports,
			company:      projectLocalPkgs,
			namedCompany: namedProjectLocalPkgs,
			project:      projectImports,
			namedProject: namedProjectImports,
		},
		blanked: blankedImports,
		dotted:  dottedImports,
	}
	return result
}

func skipPackageAlias(pkg string) string {
	values := strings.Split(pkg, " ")
	if len(values) > 1 {
		return strings.Trim(values[1], `"`)
	}

	return strings.Trim(pkg, `"`)
}

func generateFile(fset *token.FileSet, f *ast.File) ([]byte, error) {
	var output []byte
	buffer := bytes.NewBuffer(output)
	if err := printer.Fprint(buffer, fset, f); err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

func isSingleCgoImport(dd *ast.GenDecl) bool {
	if dd.Tok != token.IMPORT {
		return false
	}
	if len(dd.Specs) != 1 {
		return false
	}
	return dd.Specs[0].(*ast.ImportSpec).Path.Value == `"C"`
}

func (f *SourceFile) fixImports(
	file *ast.File,
	groups *groupsImports,
	commentsMetadata map[string]*commentsMetadata,
) {
	var importsPositions []*importPosition
	for _, decl := range file.Decls {
		dd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}

		if dd.Tok != token.IMPORT || isSingleCgoImport(dd) {
			continue
		}

		importsPositions = append(
			importsPositions, &importPosition{
				Start: dd.Pos(),
				End:   dd.End(),
			},
		)

		imports := f.importsOrders.sortImportsByOrder(groups)

		dd.Specs = rebuildImports(dd.Tok, commentsMetadata, imports)
	}

	clearImportDocs(file, importsPositions)
	removeEmptyImportNode(file)
}

// hasMultipleImportDecls will return combined import declarations to single declaration
//
// Ex.:
// import "fmt"
// import "io"
// -----
// to
// -----
// import (
//
//	"fmt"
//	"io"
//
// )
func hasMultipleImportDecls(f *ast.File) ([]ast.Decl, bool) {
	importSpecs := make([]ast.Spec, 0, len(f.Imports))
	for _, importSpec := range f.Imports {
		importSpecs = append(importSpecs, importSpec)
	}

	var (
		hasMultipleImportDecls   bool
		isFirstImportDeclDefined bool
	)

	decls := make([]ast.Decl, 0, len(f.Decls))
	for _, decl := range f.Decls {
		dd, ok := decl.(*ast.GenDecl)
		if !ok {
			decls = append(decls, decl)
			continue
		}

		if dd.Tok != token.IMPORT || isSingleCgoImport(dd) {
			decls = append(decls, dd)
			continue
		}

		if isFirstImportDeclDefined {
			hasMultipleImportDecls = true
			storedGenDecl := decls[len(decls)-1].(*ast.GenDecl)
			if storedGenDecl.Tok == token.IMPORT {
				storedGenDecl.Rparen = dd.End()
			}
			continue
		}

		dd.Specs = importSpecs
		decls = append(decls, dd)
		isFirstImportDeclDefined = true
	}

	return decls, hasMultipleImportDecls
}

func removeEmptyImportNode(f *ast.File) {
	var (
		decls      []ast.Decl
		hasImports bool
	)

	for _, decl := range f.Decls {
		dd, ok := decl.(*ast.GenDecl)
		if !ok {
			decls = append(decls, decl)

			continue
		}

		if dd.Tok == token.IMPORT && len(dd.Specs) > 0 {
			hasImports = true

			break
		}

		if dd.Tok != token.IMPORT {
			decls = append(decls, decl)
		}
	}

	if !hasImports {
		f.Decls = decls
	}
}

func rebuildImports(tok token.Token, commentsMetadata map[string]*commentsMetadata, imports [][]string) []ast.Spec {
	var specs []ast.Spec

	for i, group := range imports {
		if i != 0 && len(group) != 0 && len(specs) != 0 {
			spec := &ast.ImportSpec{Path: &ast.BasicLit{Value: "", Kind: token.STRING}}

			specs = append(specs, spec)
		}
		for _, imprt := range group {
			spec := &ast.ImportSpec{
				Path: &ast.BasicLit{Value: importWithComment(imprt, commentsMetadata), Kind: tok},
			}
			specs = append(specs, spec)
		}
	}

	return specs
}

func clearImportDocs(f *ast.File, importsPositions []*importPosition) {
	importsComments := make([]*ast.CommentGroup, 0, len(f.Comments))

	for _, comment := range f.Comments {
		for _, importPosition := range importsPositions {
			if importPosition.IsInRange(comment) {
				continue
			}
			importsComments = append(importsComments, comment)
		}
	}

	if len(f.Imports) > 0 {
		f.Comments = importsComments
	}
}

func importWithComment(imprt string, commentsMetadata map[string]*commentsMetadata) string {
	var comment string
	commentGroup, ok := commentsMetadata[imprt]
	if ok && commentGroup != nil && commentGroup.Comment != nil {
		for _, c := range commentGroup.Comment.List {
			comment += c.Text
		}
	}

	if comment == "" {
		return imprt
	}

	return fmt.Sprintf("%s %s", imprt, comment)
}

func (f *SourceFile) parseImports(file *ast.File) (map[string]*commentsMetadata, error) {
	importsWithMetadata := map[string]*commentsMetadata{}

	shouldRemoveUnusedImports := f.shouldRemoveUnusedImports
	shouldUseAliasForVersionSuffix := f.shouldUseAliasForVersionSuffix

	var packageImports map[string]string

	if shouldRemoveUnusedImports || shouldUseAliasForVersionSuffix {
		var err error
		packageImports, err = astutil.LoadPackageDependencies(filepath.Dir(f.filePath), astutil.ParseBuildTag(file))
		if err != nil {
			return nil, err
		}
	}

	for _, decl := range file.Decls {
		dd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		if isSingleCgoImport(dd) || dd.Tok != token.IMPORT {
			continue
		}
		for _, spec := range dd.Specs {
			importSpec := spec.(*ast.ImportSpec)

			if shouldRemoveUnusedImports && !astutil.UsesImport(
				file, packageImports, strings.Trim(importSpec.Path.Value, `"`),
			) {
				continue
			}

			if f.renameImports != nil {
				if newPath, ok := f.renameImports[strings.Trim(importSpec.Path.Value, `"`)]; ok {
					importSpec.Path.Value = fmt.Sprintf(`"%s"`, newPath)
				}
			}

			var importSpecStr string
			if importSpec.Name != nil {
				importSpecStr = strings.Join([]string{importSpec.Name.String(), importSpec.Path.Value}, " ")
			} else {
				if shouldUseAliasForVersionSuffix {
					importSpecStr = setAliasForVersionedImportSpec(importSpec, packageImports)
				} else {
					importSpecStr = importSpec.Path.Value
				}
			}

			importsWithMetadata[importSpecStr] = &commentsMetadata{
				Doc:     importSpec.Doc,
				Comment: importSpec.Comment,
			}
		}
	}

	return importsWithMetadata, nil
}

func setAliasForVersionedImportSpec(importSpec *ast.ImportSpec, packageImports map[string]string) string {
	var importSpecStr string

	imprt := strings.Trim(importSpec.Path.Value, `"`)
	aliasName := packageImports[imprt]

	importSuffix := path.Base(imprt)
	if importSuffix != aliasName {
		importSpecStr = fmt.Sprintf("%s %s", aliasName, importSpec.Path.Value)
	} else {
		importSpecStr = importSpec.Path.Value
	}

	return importSpecStr
}

type commentsMetadata struct {
	Doc     *ast.CommentGroup
	Comment *ast.CommentGroup
}

type importPosition struct {
	Start token.Pos
	End   token.Pos
}

func (p *importPosition) IsInRange(comment *ast.CommentGroup) bool {
	if p.Start <= comment.Pos() && comment.Pos() <= p.End {
		return true
	}

	return false
}
