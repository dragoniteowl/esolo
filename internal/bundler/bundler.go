package bundler

import (
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/evanw/esbuild/internal/ast"
	"github.com/evanw/esbuild/internal/fs"
	"github.com/evanw/esbuild/internal/lexer"
	"github.com/evanw/esbuild/internal/logging"
	"github.com/evanw/esbuild/internal/parser"
	"github.com/evanw/esbuild/internal/printer"
	"github.com/evanw/esbuild/internal/resolver"
	"github.com/evanw/esbuild/internal/runtime"
)

type file struct {
	ast ast.AST

	// If this file ends up being used in the bundle, this is an additional file
	// that must be written to the output directory. It's used by the "file"
	// loader.
	additionalFile *OutputFile

	// If true, this file was listed as not having side effects by a package.json
	// file in one of our containing directories with a "sideEffects" field.
	ignoreIfUnused bool

	// If "AbsMetadataFile" is present, this will be filled out with information
	// about this file in JSON format. This is a partial JSON file that will be
	// fully assembled later.
	jsonMetadataChunk []byte
}

type Bundle struct {
	fs          fs.FS
	res         resolver.Resolver
	sources     []logging.Source
	files       []file
	entryPoints []uint32
}

type parseFlags struct {
	isEntryPoint   bool
	isDisabled     bool
	ignoreIfUnused bool
}

type parseArgs struct {
	fs            fs.FS
	log           logging.Log
	res           resolver.Resolver
	absPath       string
	prettyPath    string
	sourceIndex   uint32
	importSource  *logging.Source
	flags         parseFlags
	pathRange     ast.Range
	parseOptions  parser.ParseOptions
	bundleOptions BundleOptions
	results       chan parseResult
}

type parseResult struct {
	source logging.Source
	file   file
	ok     bool
}

func parseFile(args parseArgs) {
	source := logging.Source{
		Index:        args.sourceIndex,
		AbsolutePath: args.absPath,
		PrettyPath:   args.prettyPath,
	}

	// Disabled files are left empty
	stdin := args.bundleOptions.Stdin
	if !args.flags.isDisabled {
		if stdin != nil {
			source.Contents = stdin.Contents
			source.PrettyPath = "<stdin>"
			if stdin.SourceFile != "" {
				source.PrettyPath = stdin.SourceFile
			}
		} else {
			var ok bool
			source.Contents, ok = args.res.Read(args.absPath)
			if !ok {
				args.log.AddRangeError(args.importSource, args.pathRange,
					fmt.Sprintf("Could not read from file: %s", args.absPath))
				args.results <- parseResult{}
				return
			}
		}
	}

	// Get the file extension
	extension := args.fs.Ext(args.absPath)

	// Pick the loader based on the file extension
	loader := args.bundleOptions.ExtensionToLoader[extension]

	// Special-case reading from stdin
	if stdin != nil {
		loader = stdin.Loader
	}

	result := parseResult{
		source: source,
		file: file{
			ignoreIfUnused: args.flags.ignoreIfUnused,
		},
		ok: true,
	}

	switch loader {
	case LoaderJS:
		result.file.ast, result.ok = parser.Parse(args.log, source, args.parseOptions)

	case LoaderJSX:
		args.parseOptions.JSX.Parse = true
		result.file.ast, result.ok = parser.Parse(args.log, source, args.parseOptions)

	case LoaderTS:
		args.parseOptions.TS.Parse = true
		result.file.ast, result.ok = parser.Parse(args.log, source, args.parseOptions)

	case LoaderTSX:
		args.parseOptions.TS.Parse = true
		args.parseOptions.JSX.Parse = true
		result.file.ast, result.ok = parser.Parse(args.log, source, args.parseOptions)

	case LoaderJSON:
		var expr ast.Expr
		expr, result.ok = parser.ParseJSON(args.log, source, parser.ParseJSONOptions{})
		result.file.ast = parser.ModuleExportsAST(args.log, source, args.parseOptions, expr)
		result.file.ignoreIfUnused = true

	case LoaderText:
		expr := ast.Expr{Data: &ast.EString{lexer.StringToUTF16(source.Contents)}}
		result.file.ast = parser.ModuleExportsAST(args.log, source, args.parseOptions, expr)
		result.file.ignoreIfUnused = true

	case LoaderBase64:
		encoded := base64.StdEncoding.EncodeToString([]byte(source.Contents))
		expr := ast.Expr{Data: &ast.EString{lexer.StringToUTF16(encoded)}}
		result.file.ast = parser.ModuleExportsAST(args.log, source, args.parseOptions, expr)
		result.file.ignoreIfUnused = true

	case LoaderDataURL:
		mimeType := mime.TypeByExtension(extension)
		if mimeType == "" {
			mimeType = http.DetectContentType([]byte(source.Contents))
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(source.Contents))
		url := "data:" + mimeType + ";base64," + encoded
		expr := ast.Expr{Data: &ast.EString{lexer.StringToUTF16(url)}}
		result.file.ast = parser.ModuleExportsAST(args.log, source, args.parseOptions, expr)
		result.file.ignoreIfUnused = true

	case LoaderFile:
		// Add a hash to the file name to prevent multiple files with the same base
		// name from colliding. Avoid using the absolute path to prevent build
		// output from being different on different machines.
		baseName := baseNameForAvoidingCollisions(args.fs, args.absPath)

		// Determine the destination folder
		targetFolder := args.bundleOptions.AbsOutputDir
		if targetFolder == "" {
			targetFolder = args.fs.Dir(args.bundleOptions.AbsOutputFile)
		}

		// Export the resulting relative path as a string
		expr := ast.Expr{ast.Loc{0}, &ast.EString{lexer.StringToUTF16(baseName)}}
		result.file.ast = parser.ModuleExportsAST(args.log, source, args.parseOptions, expr)
		result.file.ignoreIfUnused = true

		// Optionally add metadata about the file
		var jsonMetadataChunk []byte
		if args.bundleOptions.AbsMetadataFile != "" {
			jsonMetadataChunk = []byte(fmt.Sprintf(
				"{\n      \"inputs\": {},\n      \"bytes\": %d\n    }", len(source.Contents)))
		}

		// Copy the file using an additional file payload to make sure we only copy
		// the file if the module isn't removed due to tree shaking.
		result.file.additionalFile = &OutputFile{
			AbsPath:           args.fs.Join(targetFolder, baseName),
			Contents:          []byte(source.Contents),
			jsonMetadataChunk: jsonMetadataChunk,
		}

	default:
		result.ok = false
		args.log.AddRangeError(args.importSource, args.pathRange,
			fmt.Sprintf("File extension not supported: %s", args.prettyPath))
	}

	args.results <- result
}

// Identify the path by its lowercase absolute path name. This should
// hopefully avoid path case issues on Windows, which has case-insensitive
// file system paths.
func lowerCaseAbsPathForWindows(absPath string) string {
	return strings.ToLower(absPath)
}

func baseNameForAvoidingCollisions(fs fs.FS, absPath string) string {
	var toHash []byte
	if relPath, ok := fs.RelativeToCwd(absPath); ok {
		// Attempt to generate the same base name regardless of what machine or
		// operating system we're on. We want to avoid absolute paths because they
		// will have different home directories. We also want to avoid path
		// separators because they are different on Windows.
		toHash = []byte(strings.ReplaceAll(relPath, "\\", "/"))
	} else {
		// Just use the absolute path if this environment doesn't have a current
		// directory. This is the case when running tests, for example.
		toHash = []byte(absPath)
	}

	// Use "URLEncoding" instead of "StdEncoding" to avoid introducing "/"
	hashBytes := sha1.Sum(toHash)
	hash := base64.URLEncoding.EncodeToString(hashBytes[:])[:8]

	// Insert the hash before the extension
	base := fs.Base(absPath)
	ext := fs.Ext(absPath)
	return base[:len(base)-len(ext)] + "." + hash + ext
}

func ScanBundle(
	log logging.Log, fs fs.FS, res resolver.Resolver, entryPaths []string,
	parseOptions parser.ParseOptions, bundleOptions BundleOptions,
) Bundle {
	sources := []logging.Source{}
	files := []file{}
	visited := make(map[string]uint32)
	results := make(chan parseResult)
	remaining := 0

	if bundleOptions.ExtensionToLoader == nil {
		bundleOptions.ExtensionToLoader = DefaultExtensionToLoaderMap()
	}

	// Always start by parsing the runtime file
	{
		source := logging.Source{
			Index:        ast.RuntimeSourceIndex,
			AbsolutePath: "<runtime>",
			PrettyPath:   "<runtime>",
			Contents:     runtime.Code,
		}
		sources = append(sources, source)
		files = append(files, file{})
		remaining++
		go func() {
			runtimeParseOptions := parseOptions

			// Always do tree shaking for the runtime because we never want to
			// include unnecessary runtime code
			runtimeParseOptions.IsBundling = true

			ast, ok := parser.Parse(log, source, runtimeParseOptions)
			results <- parseResult{source: source, file: file{ast: ast}, ok: ok}
		}()
	}

	maybeParseFile := func(
		absPath string,
		prettyPath string,
		importSource *logging.Source,
		pathRange ast.Range,
		flags parseFlags,
	) uint32 {
		lowerAbsPath := lowerCaseAbsPathForWindows(absPath)
		sourceIndex, ok := visited[lowerAbsPath]
		if !ok {
			sourceIndex = uint32(len(sources))
			visited[lowerAbsPath] = sourceIndex
			sources = append(sources, logging.Source{})
			files = append(files, file{})
			remaining++
			go parseFile(parseArgs{
				fs:            fs,
				log:           log,
				res:           res,
				absPath:       absPath,
				prettyPath:    prettyPath,
				sourceIndex:   sourceIndex,
				importSource:  importSource,
				flags:         flags,
				pathRange:     pathRange,
				parseOptions:  parseOptions,
				bundleOptions: bundleOptions,
				results:       results,
			})
		}
		return sourceIndex
	}

	entryPoints := []uint32{}
	duplicateEntryPoints := make(map[string]bool)
	for _, absPath := range entryPaths {
		flags := parseFlags{
			isEntryPoint: true,
		}
		prettyPath := res.PrettyPath(absPath)
		lowerAbsPath := lowerCaseAbsPathForWindows(absPath)
		if duplicateEntryPoints[lowerAbsPath] {
			log.AddError(nil, ast.Loc{}, "Duplicate entry point: "+prettyPath)
			continue
		}
		duplicateEntryPoints[lowerAbsPath] = true
		sourceIndex := maybeParseFile(absPath, prettyPath, nil, ast.Range{}, flags)
		entryPoints = append(entryPoints, sourceIndex)
	}

	for remaining > 0 {
		result := <-results
		remaining--
		if !result.ok {
			continue
		}

		source := result.source
		j := printer.Joiner{}
		isFirstImport := true

		// Begin the metadata chunk
		if bundleOptions.AbsMetadataFile != "" {
			j.AddString(printer.QuoteForJSON(source.PrettyPath))
			j.AddString(fmt.Sprintf(": {\n      \"bytes\": %d,\n      \"imports\": [", len(source.Contents)))
		}

		// Don't try to resolve paths if we're not bundling
		if bundleOptions.IsBundling {
			for _, part := range result.file.ast.Parts {
				for _, importRecordIndex := range part.ImportRecordIndices {
					record := &result.file.ast.ImportRecords[importRecordIndex]

					// Don't try to resolve imports that are already resolved
					if record.SourceIndex != nil {
						continue
					}

					sourcePath := source.AbsolutePath
					pathText := record.Path.Text
					pathRange := source.RangeOfString(record.Path.Loc)
					resolveResult := res.Resolve(sourcePath, pathText)

					switch resolveResult.Status {
					case resolver.ResolveEnabled, resolver.ResolveDisabled:
						flags := parseFlags{
							isDisabled:     resolveResult.Status == resolver.ResolveDisabled,
							ignoreIfUnused: resolveResult.IgnoreIfUnused,
						}
						prettyPath := res.PrettyPath(resolveResult.AbsolutePath)
						sourceIndex := maybeParseFile(resolveResult.AbsolutePath, prettyPath, &source, pathRange, flags)
						record.SourceIndex = &sourceIndex

						// Generate metadata about each import
						if bundleOptions.AbsMetadataFile != "" {
							if isFirstImport {
								isFirstImport = false
								j.AddString("\n        ")
							} else {
								j.AddString(",\n        ")
							}
							j.AddString(fmt.Sprintf("{\n          \"path\": %s\n        }",
								printer.QuoteForJSON(prettyPath)))
						}

					case resolver.ResolveMissing:
						log.AddRangeError(&source, pathRange, fmt.Sprintf("Could not resolve %q", pathText))
					}
				}
			}
		}

		// End the metadata chunk
		if bundleOptions.AbsMetadataFile != "" {
			if !isFirstImport {
				j.AddString("\n      ")
			}
			j.AddString("]\n    }")
		}

		result.file.jsonMetadataChunk = j.Done()
		sources[source.Index] = source
		files[source.Index] = result.file
	}

	return Bundle{fs, res, sources, files, entryPoints}
}

type Loader int

const (
	LoaderNone Loader = iota
	LoaderJS
	LoaderJSX
	LoaderTS
	LoaderTSX
	LoaderJSON
	LoaderText
	LoaderBase64
	LoaderDataURL
	LoaderFile
)

func DefaultExtensionToLoaderMap() map[string]Loader {
	return map[string]Loader{
		".js":   LoaderJS,
		".mjs":  LoaderJS,
		".cjs":  LoaderJS,
		".jsx":  LoaderJSX,
		".ts":   LoaderTS,
		".tsx":  LoaderTSX,
		".json": LoaderJSON,
		".txt":  LoaderText,
	}
}

type SourceMap uint8

const (
	SourceMapNone SourceMap = iota
	SourceMapInline
	SourceMapLinkedWithComment
	SourceMapExternalWithoutComment
)

type StdinInfo struct {
	Loader     Loader
	Contents   string
	SourceFile string
}

type BundleOptions struct {
	// true: imports are scanned and bundled along with the file
	// false: imports are left alone and the file is passed through as-is
	IsBundling bool

	AbsOutputFile     string
	AbsOutputDir      string
	RemoveWhitespace  bool
	MinifyIdentifiers bool
	MangleSyntax      bool
	ModuleName        string
	ExtensionToLoader map[string]Loader
	OutputFormat      printer.Format

	// If present, metadata about the bundle is written as JSON here
	AbsMetadataFile string

	SourceMap SourceMap
	Stdin     *StdinInfo

	// If true, make sure to generate a single file that can be written to stdout
	WriteToStdout bool

	omitRuntimeForTests bool
}

type OutputFile struct {
	AbsPath  string
	Contents []byte

	// If "AbsMetadataFile" is present, this will be filled out with information
	// about this file in JSON format. This is a partial JSON file that will be
	// fully assembled later.
	jsonMetadataChunk []byte
}

type lineColumnOffset struct {
	lines   int
	columns int
}

type compileResult struct {
	printer.PrintResult

	// If this is an entry point, this is optional code to stick on the end of
	// the chunk. This is used to for example trigger the lazily-evaluated
	// CommonJS wrapper for the entry point.
	entryPointTail *printer.PrintResult

	sourceIndex uint32

	// This is the line and column offset since the previous JavaScript string
	// or the start of the file if this is the first JavaScript string.
	generatedOffset lineColumnOffset

	// The source map contains the original source code, which is quoted in
	// parallel for speed. This is only filled in if the SourceMap option is
	// enabled.
	quotedSource string
}

func (b *Bundle) Compile(log logging.Log, options BundleOptions) []OutputFile {
	if options.ExtensionToLoader == nil {
		options.ExtensionToLoader = DefaultExtensionToLoaderMap()
	}

	// The format can't be "preserve" while bundling
	if options.IsBundling && options.OutputFormat == printer.FormatPreserve {
		options.OutputFormat = printer.FormatESModule
	}

	type linkGroup struct {
		outputFiles    []OutputFile
		reachableFiles []uint32
	}

	waitGroup := sync.WaitGroup{}
	resultGroups := make([]linkGroup, len(b.entryPoints))

	// Link each file with the runtime file separately in parallel
	for i, entryPoint := range b.entryPoints {
		waitGroup.Add(1)
		go func(i int, entryPoint uint32) {
			group := &resultGroups[i]
			c := newLinkerContext(&options, log, b.fs, b.sources, b.files, []uint32{entryPoint})
			group.outputFiles = c.link()
			group.reachableFiles = c.reachableFiles
			waitGroup.Done()
		}(i, entryPoint)
	}
	waitGroup.Wait()

	// Join the results in entry point order for determinism
	var outputFiles []OutputFile
	for _, group := range resultGroups {
		outputFiles = append(outputFiles, group.outputFiles...)
	}

	// Also generate the metadata file if necessary
	if options.AbsMetadataFile != "" {
		outputFiles = append(outputFiles, OutputFile{
			AbsPath:  options.AbsMetadataFile,
			Contents: b.generateMetadataJSON(outputFiles),
		})
	}

	// Make sure an output file never overwrites an input file
	sourceAbsPaths := make(map[string]uint32)
	for _, group := range resultGroups {
		for _, sourceIndex := range group.reachableFiles {
			lowerAbsPath := lowerCaseAbsPathForWindows(b.sources[sourceIndex].AbsolutePath)
			sourceAbsPaths[lowerAbsPath] = sourceIndex
		}
	}
	for _, outputFile := range outputFiles {
		lowerAbsPath := lowerCaseAbsPathForWindows(outputFile.AbsPath)
		if sourceIndex, ok := sourceAbsPaths[lowerAbsPath]; ok {
			log.AddError(nil, ast.Loc{}, "Refusing to overwrite input file: "+b.sources[sourceIndex].PrettyPath)
		}
	}

	// Make sure an output file never overwrites another output file. This
	// is almost certainly unintentional and would otherwise happen silently.
	outputFileMap := make(map[string]bool)
	for _, outputFile := range outputFiles {
		lowerAbsPath := lowerCaseAbsPathForWindows(outputFile.AbsPath)
		if outputFileMap[lowerAbsPath] {
			outputPath := outputFile.AbsPath
			if relPath, ok := b.fs.RelativeToCwd(outputPath); ok {
				outputPath = relPath
			}
			log.AddError(nil, ast.Loc{}, "Two output files share the same path: "+outputPath)
		} else {
			outputFileMap[lowerAbsPath] = true
		}
	}

	return outputFiles
}

func (b *Bundle) generateMetadataJSON(results []OutputFile) []byte {
	// Sort files by absolute path for determinism
	sorted := make(indexAndPathArray, 0, len(b.sources))
	for sourceIndex, source := range b.sources {
		if uint32(sourceIndex) != ast.RuntimeSourceIndex {
			sorted = append(sorted, indexAndPath{uint32(sourceIndex), source.PrettyPath})
		}
	}
	sort.Sort(sorted)

	j := printer.Joiner{}
	j.AddString("{\n  \"inputs\": {")

	// Write inputs
	for i, item := range sorted {
		if i > 0 {
			j.AddString(",\n    ")
		} else {
			j.AddString("\n    ")
		}
		j.AddBytes(b.files[item.sourceIndex].jsonMetadataChunk)
	}

	j.AddString("\n  },\n  \"outputs\": {")

	// Write outputs
	isFirst := true
	for _, result := range results {
		if len(result.jsonMetadataChunk) > 0 {
			if isFirst {
				isFirst = false
				j.AddString("\n    ")
			} else {
				j.AddString(",\n    ")
			}
			j.AddString(fmt.Sprintf("%s: ", printer.QuoteForJSON(b.res.PrettyPath(result.AbsPath))))
			j.AddBytes(result.jsonMetadataChunk)
		}
	}

	j.AddString("\n  }\n}\n")
	return j.Done()
}
