//
//
//
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"

	"github.com/kuma777/go-msbuild"
)

//
// global variables
//
const (
	cbuildVersion  = "1.1.2"
	buildDirectory = "CBuild.dir"
)

var (
	option struct {
		platform            string
		targetName          string
		outputDir           string
		outputRoot          string
		verbose             bool
		ninjaFile           string
		variant             string
		templateFile        string
		useCompilerLauncher bool
	}

	useResponse     bool
	groupArchives   bool
	responseNewline bool
	useDepsMsvc     bool

	emitContext struct {
		subNinjaList      []string
		appendRules       map[string]AppendBuild
		otherRuleList     map[string]OtherRule
		commandList       []*BuildCommand
		otherRuleFileList []OtherRuleFile
		scannedConfigs    []string // remembers all scanned configuration files.
		defaultTargets    []string
	}

	project struct {
		headerFiles []string
	}

	// ProgramPath holds path to the invoked program.
	ProgramPath = getExecutablePath("cbuild")
	// ProgramName holds invoked program name.
	ProgramName = "cbuild"

	rxTruthy = regexp.MustCompile(`^\s*(?i:t(?:rue)?|y(?:es)?|on|1)(?:\s+.*)?$`)
	rxFalsy  = regexp.MustCompile(`^\s*(?i:f(?:alse)?|no?|off|0)(?:\s+.*)?$`)
)

// The entry point.
func main() {
	ProgramName = filepath.Base(ProgramPath)
	var (
		isDebug          bool
		isRelease        bool
		isProduct        bool
		isDevelop        bool
		isDevelopRelease bool
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] [<target>]\n", ProgramName)
		flag.PrintDefaults()
		os.Exit(1)
	}
	flag.BoolVar(&option.verbose, "v", false, "verbose mode")
	flag.BoolVar(&isRelease, "release", false, "release build")
	flag.BoolVar(&isDebug, "debug", true, "debug build")
	flag.BoolVar(&isDevelop, "develop", false, "develop(beta) build")
	flag.BoolVar(&isDevelopRelease, "develop_release", false, "develop(beta) release build")
	flag.BoolVar(&isProduct, "product", false, "for production build")
	flag.StringVar(&option.variant, "variant", "", "Which variant to build (debug, release...)")
	flag.StringVar(&option.platform, "type", "default", "target platform type")
	flag.StringVar(&option.targetName, "t", "", "build target name")
	flag.StringVar(&option.outputRoot, "o", "build", "build directory")
	flag.StringVar(&option.ninjaFile, "f", "build.ninja", "output build.ninja filename")
	flag.StringVar(&option.templateFile, "template", "", "Use external template file")
	flag.BoolVar(&option.useCompilerLauncher, "use-compiler-launcher", false, "Use compiler launcher")
	genMSBuild := flag.Bool("msbuild", false, "Export MSBuild project")
	projdir := flag.String("msbuild-dir", "./", "MSBuild project output directory")
	projname := flag.String("msbuild-proj", "out", "MSBuild project name")
	showVersionAndExit := flag.Bool("version", false, "display version")
	dumpDefaultTemplates := flag.Bool("show-default-template", false, "Show default template")
	flag.Parse()

	if *showVersionAndExit {
		fmt.Fprintf(os.Stdout, "%s: %v (%s/%s)\n", ProgramName, cbuildVersion, runtime.Version(), runtime.Compiler)
		os.Exit(0)
	}
	if *dumpDefaultTemplates {
		src, err := getNinjaTemplateSource("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: Failed to obtain the default template\n", ProgramName)
			os.Exit(1)
		}
		fmt.Println(src)
		os.Exit(0)
	}
	// Temporally sets option.outputDir
	option.outputDir = option.outputRoot
	if len(option.variant) == 0 {
		option.variant = Debug.String()
		if isDebug {
			option.variant = Debug.String()
		}
		if isProduct {
			option.variant = Product.String()
		}
		if isRelease {
			option.variant = Release.String()
		}
		if isDevelopRelease {
			option.variant = DevelopRelease.String()
		}
		if isDevelop {
			option.variant = Develop.String()
		}
	}
	emitContext.appendRules = make(map[string]AppendBuild)
	emitContext.otherRuleList = make(map[string]OtherRule)
	err := cbuild(*projdir, *projname, *genMSBuild)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s:error: %v\n", ProgramName, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func cbuild(projdir string, projname string, genMSBuild bool) error {
	useResponse = false
	groupArchives = false
	responseNewline = false
	useDepsMsvc = false
	if 0 < flag.NArg() && len(option.targetName) == 0 {
		option.targetName = flag.Arg(0)
	}
	if 0 < len(option.targetName) {
		Verbose("%s: Target is \"%s\"\n", ProgramName, option.targetName)
	}
	initialDictionary := importEnvironmentVariables()
	const optPrefixSym = "option_prefix"
	if _, ok := initialDictionary[optPrefixSym]; !ok {
		initialDictionary[optPrefixSym] = "-"
	}
	buildInfo := BuildInfo{
		variables:      initialDictionary,
		selectedTarget: option.targetName,
		target:         option.targetName,
	}
	if _, err := CollectConfigurations(buildInfo, ""); err != nil {
		return err
	}
	if (len(emitContext.commandList) + len(emitContext.otherRuleFileList)) <= 0 {
		fmt.Fprintf(os.Stderr, "%s: No commands to run.\n", ProgramName)
		return nil
	}
	if genMSBuild {
		Verbose("%s: Creates VC++ project file(s).\n", ProgramName)
		outputMSBuild(projdir, projname)
	} else {
		if err := outputNinja(); err != nil {
			return err
		}
		if err := outputCompileDb(); err != nil {
			return err
		}
	}
	return nil
}

// CollectConfigurations collects configurations recursively.
func CollectConfigurations(info BuildInfo, relChildDir string) ([]string, error) {
	var childPath string
	if len(relChildDir) == 0 {
		childPath = "./"
	} else {
		childPath = filepath.ToSlash(filepath.Clean(relChildDir)) + "/"
	}
	return traverse(info, childPath, 0)
}

func traverse(info BuildInfo, relChildDir string, level int) ([]string, error) {
	var err error
	if !strings.HasSuffix(relChildDir, "/") {
		return nil, errors.New("output directory should end with '/'")
	}
	Verbose("%s: Enter \"%s\"\n", ProgramName, relChildDir)
	defer Verbose("%s: Leave \"%s\"\n", ProgramName, relChildDir)

	yamlSource := filepath.Join(relChildDir, "make.yml")
	buf, err := ioutil.ReadFile(yamlSource)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read \"%s\"", yamlSource)
	}

	var conf Data
	if err = yaml.Unmarshal(buf, &conf); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal \"%s\"", yamlSource)
	}
	{
		var absPath string
		absPath, err = filepath.Abs(yamlSource)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to convert \"%s\" to absolute path", yamlSource)
		}
		emitContext.scannedConfigs = append(emitContext.scannedConfigs, filepath.ToSlash(absPath))
	}

	info.mydir = relChildDir
	//
	// select target to build.
	//
	currentTarget, targetTag, ok := chooseTarget(info, conf.Target)
	if !ok {
		return nil, errors.New("no targets")
	}
	if len(info.target) == 0 {
		info.target = currentTarget.Name
		Verbose("%s: Target is \"%s\".\n", ProgramName, info.target)
	}
	info.selectedTarget = ""

	if level == 0 && option.platform == "default" {
		option.platform = checkPlatformType(conf.Variable)
	}

	// Merge variable definitions (parent + current).

	info.variables = (func() map[string]string {
		result := make(map[string]string)
		for ik, iv := range info.variables {
			result[ik] = iv
		}
		return result
	})()

	for _, v := range conf.Variable {
		if val, ok := v.GetMatchedValue(info.target, option.platform, option.variant); ok {
			switch v.Name {
			case "enable_response":
				useResponse = ToBoolean(val)
			case "response_newline":
				responseNewline = ToBoolean(val)
			case "group_archives":
				groupArchives = ToBoolean(val)
			case "deps_msvc":
				useDepsMsvc = ToBoolean(val)
			default: /* NO-OP */
			}
			info.variables[v.Name] = val
		}
	}
	optionPrefix := info.OptionPrefix()

	if level == 0 {
		switch option.variant {
		case Product.String():
			option.outputDir = JoinPaths(option.outputRoot, option.platform, "Product")
		case Develop.String():
			option.outputDir = JoinPaths(option.outputRoot, option.platform, "Develop")
		case DevelopRelease.String():
			option.outputDir = JoinPaths(option.outputRoot, option.platform, "DevelopRelease")
		case Release.String():
			option.outputDir = JoinPaths(option.outputRoot, option.platform, "Release")
		default:
			option.outputDir = JoinPaths(option.outputRoot, option.platform, strings.Title(option.variant))
		}
	}

	info.outputdir = JoinPaths(option.outputDir, relChildDir) + "/" // Proofs '/' ending

	// Constructs include path arguments.
	for _, pth := range filterByBuildTarget(conf.Include, info.target) {
		const prefix = "$output"
		if strings.HasPrefix(pth, prefix) {
			info.AddInclude(JoinPaths(info.outputdir, "output"+pth[len(prefix):]))
			continue
		}
		asBuildRootRelative := pth[0] == '$'
		pth, err = info.StrictInterpolate(pth)
		if err != nil {
			return nil, err
		}
		if asBuildRootRelative || filepath.IsAbs(pth) {
			info.AddInclude(pth)
		} else {
			info.AddInclude(JoinPaths(relChildDir, pth))
		}
	}
	// Constructs defines.
	for _, d := range filterByBuildTarget(conf.Define, info.target) {
		info.AddDefines(d)
	}
	// Construct other options.
	for _, o := range filterByBuildTarget(conf.Option, info.target) {
		opts, err := makeOptionArgs(info, o, optionPrefix)
		if err != nil {
			return nil, err
		}
		info.options = append(info.options, opts...)
	}
	// Constructs option list for archiver.
	for _, a := range filterByBuildTarget(conf.ArchiveOption, info.target) {
		opts, err := makeOptionArgs(info, a, "")
		if err != nil {
			return nil, err
		}
		info.archiveOptions = append(info.archiveOptions, opts...)
	}
	// Constructs option list for converters.
	for _, c := range filterByBuildTarget(conf.ConvertOption, info.target) {
		opts, err := makeOptionArgs(info, c, "")
		if err != nil {
			return nil, err
		}
		info.convertOptions = append(info.convertOptions, opts...)
	}
	// Construct option list for linker.
	for _, l := range filterByBuildTarget(conf.LinkOption, info.target) {
		opts, err := makeOptionArgs(info, l, optionPrefix)
		if err != nil {
			return nil, err
		}
		info.linkOptions = append(info.linkOptions, opts...)
	}
	// Constructs system library list.
	for _, ls := range filterByBuildTarget(conf.Libraries, info.target) {
		opts, err := makeOptionArgs(info, ls, optionPrefix+"l")
		if err != nil {
			return nil, err
		}
		info.libraries = append(info.libraries, opts...)
	}
	// Constructs library list.
	for _, ld := range filterByBuildTarget(conf.LinkDepend, info.target) {
		opts, err := makeOptionArgs(info, ld, "")
		if err != nil {
			return nil, err
		}
		info.linkDepends = append(info.linkDepends, opts...)
	}
	// Constructs sub-ninjas
	for _, subninja := range filterByBuildTarget(conf.SubNinja, info.target) {
		emitContext.subNinjaList = append(emitContext.subNinjaList, subninja)
	}

	// Constructs header files.
	for _, h := range filterByBuildTarget(conf.Headers, info.target) {
		h, err = info.StrictInterpolate(h)
		if err != nil {
			return nil, err
		}
		h, _ = filepath.Abs(filepath.Join(relChildDir, h))
		project.headerFiles = append(project.headerFiles, h)
	}

	if err = registerOtherRules(&emitContext.otherRuleList, info, conf.Other); err != nil {
		return nil, err
	}

	files := filterByBuildTarget(conf.Source, info.target)
	cvfiles := filterByBuildTarget(conf.ConvertList, info.target)
	testfiles := filterByBuildTarget(conf.Tests, info.target)

	// sub-directories
	subdirs := filterByBuildTarget(conf.Subdirs, info.target)

	subArtifacts := make([]string, 0, len(subdirs))

	// Recurse into the sub-directories.
	for _, s := range subdirs {
		// relChildDir always ends with '/'
		odir := relChildDir + filepath.ToSlash(filepath.Clean(s)) + "/"
		if r, err := traverse(info, odir, level+1); err == nil {
			if 0 < len(r) {
				subArtifacts = append(subArtifacts, r...)
			}
		} else {
			return nil, err
		}
	}

	// pre build files
	cmds, err := makePreBuildCommands(info, relChildDir, conf.Prebuild)
	if err != nil {
		return nil, err
	}
	emitContext.commandList = append(emitContext.commandList, cmds...)
	// create compile list
	cmds, artifacts, err := makeCompileCommands(info, &emitContext.otherRuleList, relChildDir, files, targetTag, currentTarget.Name)
	if err != nil {
		return nil, err
	}
	emitContext.commandList = append(emitContext.commandList, cmds...)
	var result []string

	switch currentTarget.Type {
	case "library":
		// archive
		if 0 < len(artifacts) {
			// MEMO: Constructs relation
			//   <lib> 1--0..* <artifacts>
			libCmd, err := makeArchiveCommand(info, artifacts, currentTarget.Name)
			if err != nil {
				return nil, err
			}
			emitContext.commandList = append(emitContext.commandList, libCmd)
			result = append(subArtifacts, libCmd.OutFile)
			emitContext.defaultTargets = append(emitContext.defaultTargets, libCmd.OutFile)
		} else {
			Warn("There are no files to build in \"%s\".", relChildDir)
		}
	case "execute":
		// link program
		if 0 < len(artifacts) || 0 < len(subArtifacts) {
			// MEMO: Constructs relation
			//   <exe> 1--1..* <artifacts>
			//     1\
			//       +-- 1..* <artifacts from sub-directories>
			cmds, err := makeLinkCommand(
				info,
				append(artifacts, subArtifacts...),
				currentTarget.Name,
				currentTarget.Packager)
			if err != nil {
				return nil, err
			}
			emitContext.commandList = append(emitContext.commandList, cmds...)
			for _, t := range cmds {
				emitContext.defaultTargets = append(emitContext.defaultTargets, t.OutFile)
			}
		} else {
			Warn("There are no files to build in \"%s\".", relChildDir)
		}
	case "convert":
		if 0 < len(cvfiles) {
			cmd, e := makeConvertCommand(info, relChildDir, cvfiles, currentTarget.Name)
			if e != nil {
				return nil, e
			}
			emitContext.commandList = append(emitContext.commandList, cmd)
		} else {
			Warn("There are no files to convert in \"%s\".", relChildDir)
		}
	case "passthrough":
		// Just bubbling up the artifacts
		result = append(subArtifacts, artifacts...)
	case "test":
		// unit tests
		cmds, err := createTest(info, testfiles, relChildDir)
		if err != nil {
			return nil, err
		}
		emitContext.commandList = append(emitContext.commandList, cmds...)
	default:
		/* NO-OP */
	}

	Verbose("%s: Artifacts in \"%s\":\n", ProgramName, relChildDir)
	if option.verbose {
		for _, rc := range result {
			fmt.Fprintf(os.Stderr, "#   %s\n", rc)
		}
	}
	return result, nil
}

// filterByBuildTarget accumulates items associated to `buildTarget` and current build platform/variant.
func filterByBuildTarget(block []StringList, buildTarget string) []string {
	lists := make([]string, 0, len(block))
	for _, item := range block {
		lists = append(lists, item.GetMatchedItems(buildTarget, option.platform, option.variant)...)
	}
	return lists
}

func interpolateStrings(info BuildInfo, args []string) ([]string, error) {
	result := make([]string, 0, len(args))
	for _, s := range args {
		expanded, err := info.Interpolate(s)
		if err != nil {
			return nil, err
		}
		result = append(result, expanded)
	}
	return result, nil
}

// makeArchiveCommand constructs a command for creating an archive.
func makeArchiveCommand(info BuildInfo, inputs []string, libName string) (*BuildCommand, error) {
	arCommand, e := info.ExpandVariable("archiver")
	if e != nil {
		return nil, e
	}

	cmd := BuildCommand{
		Command:     arCommand,
		CommandType: "ar",
		Args:        info.archiveOptions,
		InFiles:     inputs,
		Project:     libName,
		OutFile: (func() string {
			switch option.platform {
			case "WIN32":
				return JoinPaths(info.outputdir, libName+".lib")
			default:
				return JoinPaths(info.outputdir, fmt.Sprintf("lib%s.a", libName))
			}
		})(),
		NeedCommandAlias: true,
	}

	return &cmd, nil
}

// makeLinkCommand constructs a command for building/packaging an executable.
func makeLinkCommand(
	info BuildInfo,
	sourceArtifacts []string,
	executableName string,
	packager Packager) ([]*BuildCommand, error) {

	var err error

	result := make([]*BuildCommand, 0, 1)
	targetPath := JoinPaths(info.MakeExecutablePath(executableName))

	linker, err := info.ExpandVariable("linker")
	if err != nil {
		return result, err
	}

	options := make([]string, 0, len(info.linkOptions))
	for _, lo := range info.linkOptions {
		lo = strings.Replace(lo, "$out", targetPath, -1)
		options = append(options, lo)
	}
	options = append(options, info.libraries...)

	// execute
	cmd := BuildCommand{
		Command:          linker,
		CommandType:      "link",
		Args:             options,
		InFiles:          sourceArtifacts,
		OutFile:          targetPath,
		Depends:          info.linkDepends,
		NeedCommandAlias: true,
		Project:          info.target,
	}
	result = append(result, &cmd)
	//fmt.Println("-o " + NowTarget.Name + flist)

	if 0 < len(packager.Target) {
		// package
		packageName := JoinPaths(option.outputDir, executableName, packager.Target)
		var (
			packagerBin  string
			packagerArgs []string
		)

		packagerBin, err = info.ExpandVariable("packager")
		if err != nil {
			return result, err
		}
		packagerArgs, err = interpolateStrings(info, strings.Split(packager.Option, " "))
		if err != nil {
			return result, err
		}
		pkg := BuildCommand{
			Command:          packagerBin,
			CommandType:      "packager",
			Args:             packagerArgs,
			InFiles:          []string{targetPath},
			OutFile:          packageName,
			NeedCommandAlias: true}
		result = append(result, &pkg)
	}
	return result, err
}

//
// convert objects
//
// makeConvertCommand constructs a command for user defined conversion operations.
func makeConvertCommand(
	info BuildInfo,
	srcDir string,
	sources []string,
	targetName string) (*BuildCommand, error) {

	outFile := JoinPaths(info.outputdir, targetName)
	converterBin, ok := info.variables["converter"]
	if !ok {
		return nil, errors.Errorf("missing the `converter` definitions for \"%s\"", targetName)
	}

	inFiles := make([]string, 0, len(sources))
	for _, f := range sources {
		inFiles = append(inFiles, JoinPaths(srcDir, f))
	}

	cmd := BuildCommand{
		Command:          converterBin,
		CommandType:      "convert",
		Args:             info.convertOptions,
		InFiles:          inFiles,
		OutFile:          outFile,
		NeedCommandAlias: true,
		Project:          info.target,
	}
	return &cmd, nil
}

//
// unit tests
//
func createTest(info BuildInfo, inputs []string, loaddir string) ([]*BuildCommand, error) {
	carg := append(info.includes, info.defines...)
	result := make([]*BuildCommand, 0, len(inputs))
	for _, ca := range info.options {
		switch ca {
		case "$out":
		case "$dep":
		case "$in":
		case "-c":
			/* NO-OP */
		default:
			carg = append(carg, ca)
		}
	}
	//objdir := filepath.Join(info.outputdir, buildDirectory) + "/"
	//objdir := info.outputdir

	for _, f := range inputs {
		// first, compile a test driver
		objcmds, artifacts, err := makeCompileCommands(info, &emitContext.otherRuleList, loaddir, []string{f}, "", "")
		if err != nil {
			return nil, errors.Wrap(err, "failed to construct a commmand")
		}
		result = append(result, objcmds...)
		// then link it as an executable (test_aaa.cpp -> test_aaa)
		cmds, err := makeLinkCommand(
			info,
			artifacts,
			info.MakeExecutablePath(strings.TrimSuffix(f, filepath.Ext(f))),
			Packager{})
		if err != nil {
			return result, errors.Wrapf(err, "failed to construct a command for testing")
		}
		result = append(result, cmds...)
	}
	return result, nil
}

// makeOptionArgs constructs option arguments from rawOpts.
// All variable references are resolved here.
func makeOptionArgs(info BuildInfo, rawOpts string, optionPrefix string) ([]string, error) {
	options := strings.Split(optionPrefix+rawOpts, " ")
	result := make([]string, 0, len(options))

	for _, opt := range options {
		s, err := info.StrictInterpolate(opt)
		if err != nil {
			return result, err
		}
		if strings.ContainsAny(s, " \t") {
			result = append(result, fmt.Sprintf(`"%s"`, s))
		} else {
			result = append(result, s)
		}
	}
	return result, nil
}

// chooseTarget chooses a target matching current build configuration.
func chooseTarget(info BuildInfo, candidates []Target) (target Target, tag string, found bool) {
	makeTag := func(s string) string {
		return "_" + s
	}
	if 0 < len(info.selectedTarget) {
		// search target
		for _, t := range candidates {
			if info.selectedTarget == t.Name {
				return t, makeTag(t.Name), true
			}
		}
		return
	}

	if 0 < len(info.target) {
		// search by_target
		for _, t := range candidates {
			if info.target == t.ByTarget {
				return t, makeTag(info.target), true
			}
		}
		// search target
		for _, t := range candidates {
			if info.target == t.Name {
				return t, makeTag(info.target), true
			}
		}
	}
	if 0 < len(candidates) {
		t := candidates[0]
		if len(info.target) == 0 {
			return t, makeTag(t.Name), true
		}
		return t, "", true
	}
	return
}

// FixupCommandPath fixes command path (appeared at the 1st element).
// Returns fixed command-line and command path
func FixupCommandPath(command string, commandDir string) (commandLine string, commandPath string) {
	args := strings.Split(command, " ")
	cmd := args[0]
	commandPath = JoinPaths(commandDir, cmd)
	args[0] = commandPath
	commandLine = strings.Join(args, " ")
	return
}

// makePreBuildCommands constructs a command list for preparing later builds.
func makePreBuildCommands(info BuildInfo, loaddir string, buildItems []Build) ([]*BuildCommand, error) {
	result := make([]*BuildCommand, 0)
	for _, build := range buildItems {
		if !build.Match(info.target, option.platform) {
			continue
		}

		// register prebuild
		sources := filterByBuildTarget(build.Source, info.target)
		if len(sources) == 0 {
			return result, errors.Errorf("no sources for command `%s`", build.Name)
		}
		{
			// Fixes source elements
			newSources := sources[:0]
			for _, src := range sources {
				switch {
				case src[0] == '$':
					sabs, _ := filepath.Abs(filepath.Join(info.outputdir, "output", src[1:]))
					src = JoinPaths(sabs)
				case src == "always":
					src = "always |"
				default:
					expanded, err := info.Interpolate(src)
					if err != nil {
						return result, err
					}
					src = JoinPaths(loaddir, expanded)
				}
				newSources = append(newSources, src)
			}
		}
		buildCommand, ok := info.variables[build.Command]
		if !ok {
			return result,
				errors.Errorf("missing build command \"%s\" (referenced from \"%s\")",
					build.Command, build.Name)
		}
		buildCommand, err := info.Interpolate(strings.Replace(buildCommand, "${selfdir}", loaddir, -1))
		if err != nil {
			return result, err
		}
		commandLabel := (func(cmd string) string {
			s := fmt.Sprintf("%s.%s", cmd, filepath.ToSlash(filepath.Clean(info.outputdir)))
			return strings.Replace(s, "/", "_", -1)
		})(build.Command)
		deps := []string{}

		if _, ok := emitContext.appendRules[commandLabel]; !ok {
			// Create a rule...
			// Fixes command path.
			switch {
			case buildCommand[0] == '$':
				r, d := FixupCommandPath(buildCommand[1:], info.outputdir)
				abs, err := filepath.Abs(d)
				if err != nil {
					return result, errors.Wrapf(err, "failed to obtain the absolute path for \"%s\"", d)
				}
				d = filepath.ToSlash(abs)
				deps = append(deps, d)
				buildCommand = r
			case strings.HasPrefix(buildCommand, "../"), strings.HasPrefix(buildCommand, "./"):
				r, d := FixupCommandPath(buildCommand, loaddir)
				deps = append(deps, d)
				buildCommand = r
			}
			emitContext.appendRules[commandLabel] = AppendBuild{
				Command: strings.Replace(buildCommand, "$target", info.target, -1),
				Desc:    build.Command,
				Deps:    0 < len(build.Deps),
			}
		}

		if build.Name[0] != '$' || strings.HasPrefix(build.Name, "$target/") {
			pn := build.Name
			if pn[0] == '$' { // bulid.Name is "$target/..."
				pn = strings.Replace(pn, "$target/", fmt.Sprintf("/.%s/", info.target), 1)
			}
			outfile, err := filepath.Abs(filepath.Join(info.outputdir, pn))
			if err != nil {
				return nil,
					errors.Wrapf(err, "failed to obtain the absolute path for \"%s\"",
						filepath.Join(info.outputdir, pn))
			}
			cmd := BuildCommand{
				Command:          build.Command,
				CommandType:      commandLabel,
				Depends:          deps,
				InFiles:          sources,
				OutFile:          JoinPaths(outfile),
				NeedCommandAlias: false,
				Project:          info.target,
			}
			//commandList = append(commandList, &cmd)
			result = append(result, &cmd)
		} else {
			// Found `$...` -> extension specific rules.
			ext := build.Name[1:]
			for _, src := range sources {
				relPath := filepath.Join(info.outputdir, "output", Basename(src, filepath.Ext(src))+ext)
				outfile, err := filepath.Abs(relPath)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to obtain the absolute path for \"%s\"", relPath)
				}
				cmd := BuildCommand{
					Command:          build.Command,
					CommandType:      commandLabel,
					Depends:          deps,
					InFiles:          []string{src},
					OutFile:          JoinPaths(outfile),
					NeedCommandAlias: false,
					Project:          info.target,
				}
				//commandList = append(commandList, &cmd)
				result = append(result, &cmd)
			}
		}
	}
	return result, nil
}

// ReplaceExtension replaces extension to `ext`
// Accepts both `.EXT` and `EXT` as a new extension.
func ReplaceExtension(path string, ext string) string {
	result := filepath.Base(path)
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	e := filepath.Ext(path)
	if sz := len(e); 0 < sz {
		return result[:len(result)-sz] + ext
	}
	return result + ext
}

// Basename obtains the basename of the `path`
// If called as `Basename ("foo.bar", ".bar")`, Basename returns "foo" (strip matched extension)
func Basename(path string, args ...string) string {
	p := filepath.Base(path)
	if len(args) == 0 {
		return p
	}
	ext := args[0]
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	e := filepath.Ext(p)
	if e == ext {
		// Extension matched, trim it.
		return p[:len(p)-len(e)]
	}
	return p
}

// Build command for compiling C, C++...
// Returns command and artifact list.
func makeCompileCommands(
	info BuildInfo,
	otherDict *map[string]OtherRule,
	loaddir string, files []string, targetTag, projectName string) (result []*BuildCommand, artifactPaths []string, err error) {

	if len(files) == 0 {
		return
	}
	artifactPaths = make([]string, 0, len(files))
	result = make([]*BuildCommand, 0, len(files))

	compiler, err := info.ExpandVariable("compiler")
	if err != nil {
		return result, artifactPaths, errors.Wrapf(err, "missing ${compiler} definitions")
	}
	pchCmd, err := createPCH(info, loaddir, compiler, targetTag)
	if err != nil {
		return result, artifactPaths, err
	}
	if pchCmd != nil {
		result = append(result, pchCmd)
	}

	arg1 := append(info.includes, info.defines...)

	for _, srcPath := range files {
		dstPathBase := srcPath // `dstPathBase` contains the basename of the `srcPath`.
		var objdir string
		if srcPath[0] == '$' {
			// Auto generated paths.
			if strings.HasPrefix(srcPath, "$target/") {
				dstPathBase =
					strings.Replace(dstPathBase, "$target/", fmt.Sprintf("/.%s/", info.target), 1)
			} else {
				dstPathBase = srcPath[1:]
			}
			srcPath = filepath.Join(info.outputdir, dstPathBase)
			dstPathBase = filepath.Base(dstPathBase)
			objdir = JoinPaths(filepath.Dir(srcPath), buildDirectory+targetTag)
		} else {
			// At this point, `srcPath` is a relative path rooted from `loaddir`
			tf := filepath.Join(loaddir, srcPath)
			objdir = JoinPaths(filepath.Dir(filepath.Join(info.outputdir, srcPath)), buildDirectory+targetTag)
			dstPathBase = filepath.Base(tf)
			srcPath = tf
		}
		srcPath, _ = filepath.Abs(srcPath)
		srcName := JoinPaths(srcPath)
		objName := JoinPaths(objdir, dstPathBase+".o")
		depName := JoinPaths(objdir, dstPathBase+".d")

		artifactPaths = append(artifactPaths, objName)

		carg := make([]string, 0, len(arg1)+len(info.options))
		carg = append(carg, arg1...)
		for _, ca := range info.options {
			switch ca {
			case "$out":
				ca = objName
			case "$dep":
				ca = depName
			case "$in":
				ca = srcName
			default:
				/* NO-OP */
			}
			carg = append(carg, ca)
		}
		srcExt := filepath.Ext(srcPath)
		targetProject := projectName
		if targetProject == "" {
			targetProject = info.target
		}
		if rule, exists := (*otherDict)[srcExt]; exists {
			// Custom rules
			if customCompiler, ok := info.variables[rule.Compiler]; ok {
				customCompiler, err = info.Interpolate(customCompiler)
				if err != nil {
					return result,
						artifactPaths,
						errors.Wrapf(err, "failed to obtain the compiler definition from \"%s\"",
							rule.Compiler)
				}
				ocmd := OtherRuleFile{
					Rule:     "compile" + srcExt,
					Compiler: customCompiler,
					Infile:   srcName,
					Outfile:  objName,
					Include: (func() string {
						if !rule.needInclude {
							return ""
						}
						return strings.Join(info.includes, " ")
					})(),
					Option: (func() string {
						opts := make([]string, 0, len(rule.Options))
						for _, o := range rule.Options {
							switch o {
							case "$out":
								opts = append(opts, objName)
							case "$in":
								opts = append(opts, srcName)
							case "$dep":
								opts = append(opts, depName)
							default:
								opts = append(opts, o)
							}
						}
						return strings.Join(opts, " ")
					})(),
					Define: (func() string {
						if !rule.needDefine {
							return ""
						}
						return strings.Join(info.defines, " ")
					})(),
					Project: targetProject,
				}
				if rule.NeedDepend == true {
					ocmd.Depend = depName
				}
				emitContext.otherRuleFileList = append(emitContext.otherRuleFileList, ocmd) // Record it
			} else {
				Warn("compiler: Missing a compiler \"%s\" definitions in \"%s\".",
					rule.Compiler,
					JoinPaths(info.mydir, "make.yml"))
			}
		} else {
			// normal
			cmd := BuildCommand{
				Command:          compiler,
				CommandType:      "compile",
				Args:             carg,
				InFiles:          []string{srcName},
				OutFile:          objName,
				DepFile:          depName,
				NeedCommandAlias: true,
				Project:          targetProject,
			}
			if pchCmd != nil {
				cmd.ImplicitDepends = append(cmd.ImplicitDepends, pchCmd.OutFile)
				cmd.Args = append(cmd.Args, "-include-pch", pchCmd.OutFile)
			}
			result = append(result, &cmd)
			subExt := func(s string, newExt string) string {
				ex := filepath.Ext(s)
				if len(ex) == 0 {
					return s + newExt
				}
				return s[:len(s)-len(ex)] + newExt
			}
			analyzeCmd := BuildCommand{
				Command:          compiler,
				CommandType:      "analyze",
				Args:             carg,
				InFiles:          []string{srcName},
				OutFile:          subExt(objName, ".report"),
				DepFile:          subExt(depName, ".report-d"),
				NeedCommandAlias: true,
				Project:          targetProject,
			}
			if pchCmd != nil {
				analyzeCmd.ImplicitDepends = append(cmd.ImplicitDepends, pchCmd.OutFile)
				analyzeCmd.Args = append(analyzeCmd.Args, "-include-pch", pchCmd.OutFile)
			}
			result = append(result, &analyzeCmd)
		}
	}
	return result, artifactPaths, nil
}

// Create pre-compiled header if possible.
func createPCH(info BuildInfo, srcdir string, compiler string, targetTag string) (*BuildCommand, error) {
	const pchName = "00-common-prefix.hpp"
	pchSrc := JoinPaths(srcdir, pchName)
	if !Exists(pchSrc) {
		Verbose("%s: \"%s\" is not detected.\n", ProgramName, pchSrc)
		return nil, nil
	}
	Verbose("%s: \"%s\" found.\n", ProgramName, pchSrc)
	pchDst := JoinPaths(info.outputdir, srcdir, buildDirectory+targetTag, pchName+".pch")
	Verbose("%s: Create PCH \"%s\"\n", ProgramName, pchDst)
	args := append(info.includes, info.defines...)
	for _, opt := range info.options {
		args = append(args, (func(o string) string {
			switch o {
			case "$out":
				return pchDst
			case "$dep":
				return pchDst + ".dep"
			case "$in":
				return pchSrc
			default:
				return o
			}
		})(opt))
	}
	// PCH source found.
	Verbose("%s: PCH creation command line is \"%s\".\n", ProgramName, strings.Join(args, " "))
	cmd := BuildCommand{
		Command:          compiler,
		CommandType:      "gen_pch",
		Args:             args,
		InFiles:          []string{pchSrc},
		OutFile:          pchDst,
		DepFile:          pchDst + ".dep",
		NeedCommandAlias: true,
		Project:          info.target,
	}
	return &cmd, nil
}

// Exists checks `filename` existence.
func Exists(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

// Registers custom rules.
func registerOtherRules(dict *map[string]OtherRule, info BuildInfo, others []Other) error {
	optPrefix := info.OptionPrefix()
	for _, ot := range others {
		if !ot.MatchPlatform(option.platform) {
			continue
		}

		ext := ot.Extension

		var optlist []string
		for _, o := range filterByBuildTarget(ot.Option, info.target) {
			ol, err := makeOptionArgs(info, o, optPrefix)
			if err != nil {
				return errors.Wrapf(err, "failed to construct option list for custom rules")
			}
			optlist = append(optlist, ol...)
		}

		needInclude := false
		needOption := false
		needDefine := false
		rule, ok := (*dict)[ext]
		if ok {
			rule.Options = append(rule.Options, optlist...)
		} else {
			// no exist rule
			cmdl := strings.Split(ot.Command, " ")
			if len(cmdl) == 0 {
				return errors.Errorf("no commands to \"%s\"", ext)
			}
			compiler := cmdl[0]

			commands := cmdl[:0] // [Filter w/o allocating](https://github.com/golang/go/wiki/SliceTricks#filtering-without-allocating)
			commands = append(commands, "$compiler")
			for _, c := range cmdl[1:] {
				switch c {
				case "@include":
					needInclude = true
					commands = append(commands, "$include")
				case "@option":
					needOption = true
					commands = append(commands, "$option")
				case "@define":
					needDefine = true
					commands = append(commands, "$define")
				default:
					commands = append(commands, c)
				}
			}

			rule = OtherRule{
				Compiler:    compiler,
				Command:     strings.Join(commands, " "),
				Title:       ot.Description,
				Options:     optlist,
				needInclude: needInclude,
				needOption:  needOption,
				needDefine:  needDefine,
				NeedDepend:  ot.NeedDepend,
			}
		}
		(*dict)[ext] = rule
	}
	return nil
}

func checkPlatformType(vlist []Variable) string {
	for _, v := range vlist {
		if v.Name == "default_type" {
			return v.Value
		}
	}
	return "default"
}

// Creates *.ninja file.
func outputNinja() error {
	Verbose("%s: Creates \"%s\"\n", ProgramName, option.ninjaFile)

	var err error

	tDir := filepath.Dir(option.ninjaFile)

	if !Exists(tDir) {
		err = os.MkdirAll(tDir, 0755)
		if err != nil {
			return err
		}
	}
	file, err := ioutil.TempFile(tDir, "ninja-")
	defer (func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	})()
	Verbose("%s: Creating transient output \"%s\"\n", ProgramName, file.Name())
	sink := bufio.NewWriter(file)

	tmpl, err := getNinjaTemplate(option.templateFile)
	if err != nil {
		return errors.Wrapf(err, "failed to obtain a template")
	}

	// Emits rules for updating `build.ninja`
	type WriteContext struct {
		TemplateFile       string
		Platform           string
		OutputDirectory    string
		OtherRules         map[string]OtherRule
		AppendRules        map[string]AppendBuild
		NinjaUpdater       string
		UsePCH             bool
		UseDepsMsvc        bool
		UseResponse        bool
		NewlineAsDelimiter bool
		GroupArchives      bool
		CompilerLauncher   string

		Commands         []*BuildCommand
		OtherRuleTargets []OtherRuleFile
		SubNinjas        []string
		NinjaFile        string
		ConfigSources    []string
		AnalysisReports  []string
		DefaultTargets   []string
	}
	osArgs := make([]string, 0, len(os.Args))
	osArgs = append(osArgs, filepath.ToSlash(os.Args[0]))
	osArgs = append(osArgs, os.Args[1:]...)
	launcher := ""
	if option.useCompilerLauncher {
		launcher = FindCompilerLauncher()
		if launcher != "" {
			launcher = "\"" + launcher + "\" -p $project"
		}
	}
	ctx := WriteContext{
		TemplateFile:       option.templateFile,
		Platform:           option.platform,
		UseResponse:        useResponse,
		NewlineAsDelimiter: responseNewline,
		GroupArchives:      groupArchives,
		OutputDirectory:    filepath.ToSlash(option.outputDir),
		OtherRules:         emitContext.otherRuleList,
		AppendRules:        emitContext.appendRules,
		UsePCH:             true,
		UseDepsMsvc:        useDepsMsvc,
		NinjaUpdater:       strings.Join(osArgs, " "),
		CompilerLauncher:   launcher,

		Commands:         emitContext.commandList,
		OtherRuleTargets: emitContext.otherRuleFileList,
		SubNinjas:        emitContext.subNinjaList,
		NinjaFile:        option.ninjaFile,
		ConfigSources:    emitContext.scannedConfigs,
		DefaultTargets:   emitContext.defaultTargets,
	}
	for _, f := range emitContext.commandList {
		if f.CommandType != "analyze" {
			continue
		}
		ctx.AnalysisReports = append(ctx.AnalysisReports, f.OutFile)
	}
	err = tmpl.Execute(sink, ctx)
	if err != nil {
		return errors.Wrap(err, "failed to render template")
	}

	sink.Flush()

	if err := file.Close(); err != nil {
		return errors.Wrapf(err, "closing \"%s\" failed.", file.Name())
	}
	if err := os.Rename(file.Name(), option.ninjaFile); err != nil {
		return errors.Wrapf(err, "renaming \"%s\" to \"%s\" failed.", file.Name(), option.ninjaFile)
	}
	Verbose("%s: Renaming %s to %s\n", ProgramName, file.Name(), option.ninjaFile)
	return nil
}

func getNinjaTemplate(path string) (*template.Template, error) {
	const rootName = "root"

	funcs := template.FuncMap{
		"escape_drive":         escapeDriveColon,
		"join":                 strings.Join,
		"intercalate":          intercalate,
		"substitute_extension": substituteExtension,
	}

	src, err := getNinjaTemplateSource(path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load template \"%s\"", path)
	}
	return template.New(rootName).Funcs(funcs).Parse(src)
}

func getNinjaTemplateSource(path string) (string, error) {
	if 0 < len(path) && Exists(path) {
		b, err := ioutil.ReadFile(path)
		if err != nil {
			return "", errors.Wrapf(err, "failed to load template \"%s\"", path)
		}
		return string(b), nil
	}
	defaultTemplate := `# AUTOGENERATED using built-in template
# Rule definitions
builddir = {{.OutputDirectory}}

rule compile
    description = Compiling: $desc
{{- if eq .Platform "WIN32"}}
    command = {{.CompilerLauncher}} "$compile" $options -Fo$out $in
    {{- if .UseDepsMsvc}}
    deps = msvc
    {{- else}}
    depfile = $depf
    deps = gcc
    {{- end}}
{{- else}}
    command = {{.CompilerLauncher}} "$compile" $options -o $out $in
    depfile = $depf
    deps = gcc
{{end}}

rule analyze
    description = Analyzing: $desc
    command = $analyze $options --analyze -Xanalyzer -analyzer-output=plist-multi-file -o $out $in
    depfile = $depf
    deps = gcc

{{- if .UsePCH}}
rule gen_pch
    description = Create PCH: $desc
    command = $gen_pch $options -x c++-header -o $out $in
    depfile = $depf
    deps = gcc
{{- end}}

rule ar
    description = Archiving: $desc
{{- if .UseResponse}}
    command = $ar $options {{if eq .Platform "WIN32"}}/out:$out{{else}}$out{{end}} @$out.rsp
    rspfile = $out.rsp
    rspfile_content = {{if .NewlineAsDelimiter}}$in_newline{{else}}$in{{end}}
{{- else}}
    command = {{.CompilerLauncher}} "$ar" $options $out $in
{{- end}}

rule link
    description = Linking: $desc
{{- if .UseResponse}}
    {{- if eq .Platform "WIN32"}}
    command = $link $options /out:$out @$out.rsp
    {{- else}}
    command = $link $options -o $out @$out.rsp
    {{- end}}
    rspfile = $out.rsp
    rspfile_content = {{if .NewlineAsDelimiter}}$in_newline{{else}}$in{{end}}
{{- else}}
    {{- if .GroupArchives}}
    command = $link $options -o $out -Wl,--start-group $in -Wl,--end-group
    {{- else}}
    command = $link -o $out $in $options
    {{- end}}
{{- end}}

rule packager
    description = Packaging: $desc
    command = $packager $options $in $out

rule convert
    description = Converting: $desc
    command = $convert $options -o $out $in
{{range $k, $v := .OtherRules}}
rule compile{{- $k}}
    description = {{$v.Title}}: $desc
    command = {{$v.Command}}
    {{- if $v.NeedDepend}}
    depfile = $depf
    deps = gcc
    {{- end}}
{{end}}
{{- range $k, $v := .AppendRules}}
rule {{$k}}
    description = {{$v.Desc}}: $desc
    command = {{$v.Command}}
    {{- if $v.Deps}}
    depfile = $out.d
    deps = gcc
    {{- end}}
{{end}}
rule update_ninja_file
    description = Update $desc
    command     = {{.NinjaUpdater}}
    generator   = 1

build always: phony

build analyze-all : phony {{.AnalysisReports | escape_drive | intercalate " "}}

# end of [Rule definitions]

{{- define "IMPDEPS_"}}
    {{- if .}} | {{escape_drive . | intercalate " "}}{{end}}
{{- end}}
{{/* Render rules */}}

# Commands
build {{.NinjaFile | escape_drive}} : update_ninja_file {{escape_drive .ConfigSources | intercalate " "}}
    desc = {{.NinjaFile}}
{{range $c := .Commands}}
build {{$c.OutFile | escape_drive}} : {{$c.CommandType}} {{escape_drive $c.InFiles | intercalate " "}} {{escape_drive $c.Depends | intercalate " "}} {{template "IMPDEPS_" $c.ImplicitDepends}}
    desc = {{$c.OutFile}}
{{- if $c.NeedCommandAlias}}
    {{$c.CommandType}} = {{$c.Command}}
{{- end}}
{{- if $c.DepFile}}
    depf = {{$c.DepFile}}
    deps = gcc
{{- end}}
{{- if $c.Args}}
    options = {{intercalate " " $c.Args}}
{{- end}}
{{- if $c.Project}}
    project = {{$c.Project}}
{{- end}}
{{end}}

# Other targets
{{range $item := .OtherRuleTargets}}
build {{$item.Outfile | escape_drive}} : {{$item.Rule}} {{escape_drive $item.Infile}}
    desc     = {{$item.Outfile}}
    compiler = {{$item.Compiler}}
{{- if $item.Include}}
    include  = {{$item.Include}}
{{- end}}
{{- if  $item.Option}}
    option   = {{$item.Option}}
{{- end}}
{{- if $item.Define}}
    define   = {{$item.Define}}
{{- end}}
{{- if $item.Depend}}
    depf     = {{$item.Depend}}
{{- end}}
{{- if $item.Project}}
    project = {{$item.Project}}
{{- end}}
{{end}}
{{- if .SubNinjas}}
{{range $subninja := .SubNinjas}}
subninja {{$subninja}}
{{end}}
{{end}}
default {{.DefaultTargets | escape_drive | intercalate " "}}
`
	return defaultTemplate, nil
}

// Creates *.vcxproj (for VisualStudio).
func outputMSBuild(outdir, projname string) error {
	targets := make([]string, 0, len(emitContext.commandList)+len(project.headerFiles))
	for _, command := range emitContext.commandList {
		if command.CommandType != "compile" {
			continue
		}
		targets = append(targets, command.InFiles...)
	}
	targets = append(targets, project.headerFiles...)
	msbuild.ExportProject(targets, outdir, projname)
	return nil
}

func outputCompileDb() error {
	ninjaDir, err := filepath.Abs(filepath.Dir(option.ninjaFile))
	if err != nil {
		return err
	}
	ninjaDir = filepath.ToSlash(ninjaDir)
	if !Exists(option.outputDir) {
		err := os.MkdirAll(option.outputDir, 0755)
		if err != nil {
			return errors.Wrapf(err, "failed to create directory \"%s\"", option.outputDir)
		}
	}
	outPath := filepath.Join(option.outputDir, "compile_commands.json")
	items := make([]CompileDbItem, 0, len(emitContext.commandList))
	for _, c := range emitContext.commandList {
		if c.CommandType != "compile" || len(c.Args) == 0 {
			continue
		}
		infile := c.InFiles[0]
		if p, err := filepath.Rel(ninjaDir, infile); err == nil {
			infile = filepath.ToSlash(p)
		}
		args := make([]string, 0, 1+len(c.Args))
		args = append(args, c.Command)
		args = append(args, c.Args...)
		args = append(args, "-o", c.OutFile, infile)
		items = append(
			items,
			CompileDbItem{
				File:      infile,
				Directory: ninjaDir,
				Output:    c.OutFile,
				Arguments: args,
			})
	}
	return CreateCompileDbFile(outPath, items)
}

// JoinPaths joins suppiled path components and normalize the result.
func JoinPaths(paths ...string) string {
	return filepath.ToSlash(filepath.Clean(filepath.Join(paths...)))
}

func intercalate(sep string, arg interface{}) (string, error) {
	switch v := arg.(type) {
	case []string:
		return strings.Join(v, sep), nil
	case []interface{}:
		tmp := make([]string, 0, len(v))
		for _, s := range v {
			if ss, ok := s.(fmt.Stringer); ok {
				tmp = append(tmp, ss.String())
			}
		}
		return strings.Join(tmp, sep), nil
	}
	return "", errors.Errorf("can't convert \"%v\" to string", arg)
}

// Escapes ':' in path.
func escapeDriveColon(arg interface{}) (interface{}, error) {
	switch v := arg.(type) {
	case string:
		return escapeDriveColon1(v), nil
	case []string:
		tmp := make([]string, 0, len(v))
		for _, p := range v {
			tmp = append(tmp, escapeDriveColon1(p))
		}
		return tmp, nil
	default:
		if s, ok := arg.(fmt.Stringer); ok {
			return escapeDriveColon1(s.String()), nil
		}
	}
	return "", errors.Errorf("can't convert \"%v\" to string", arg)
}

func escapeDriveColon1(p string) string {
	if filepath.IsAbs(p) && strings.Index(p, ":") == 1 {
		drive := filepath.VolumeName(p)
		if 0 < len(drive) {
			drive = strings.Replace(strings.ToLower(drive), ":", "$:", 1)
			p = drive + p[2:]
		}
	}
	return p
}

func substituteExtension(ext string, arg interface{}) (interface{}, error) {
	switch v := arg.(type) {
	case string:
		return ReplaceExtension(v, ext), nil
	case []string:
		tmp := make([]string, 0, len(v))
		for _, p := range v {
			tmp = append(tmp, ReplaceExtension(p, ext))
		}
		return tmp, nil
	default:
		if s, ok := arg.(fmt.Stringer); ok {
			return ReplaceExtension(s.String(), ext), nil
		}
	}
	return nil, errors.Errorf("can't convert \"%v\" to string", arg)
}

// Verbose output if wanted
func Verbose(format string, args ...interface{}) {
	if option.verbose {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

// Warn emits a warning to `os.Stderr`
func Warn(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "%s:warning:", ProgramName)
	fmt.Fprintf(os.Stderr, format, args...)
	fmt.Fprintln(os.Stderr)
}

// ToBoolean converts passed string to boolean.
func ToBoolean(s string) bool {
	if rxTruthy.MatchString(s) {
		return true
	}
	if rxFalsy.MatchString(s) {
		return false
	}
	Warn("Ambiguous boolean \"%s\" found", s)
	return false
}

// Obtains executable path if possible.
func getExecutablePath(defaultName string) string {
	if n, err := os.Executable(); err == nil {
		return filepath.ToSlash(n)
	}
	return defaultName
}

func importEnvironmentVariables() map[string]string {
	result := make(map[string]string)
	for _, env := range os.Environ() {
		values := strings.SplitN(env, "=", 2)
		switch len(values) {
		case 0:
			continue
		case 1:
			result[env] = ""
		default:
			result[values[0]] = filepath.ToSlash(values[1]) // Kludge! should be avoided if possible.
		}
	}
	return result
}
