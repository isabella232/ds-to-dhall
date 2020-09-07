package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/inconshreveable/log15"
	flag "github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

var destinationFile string
var typeFile string
var timeout time.Duration
var useK8sSchema bool
var ignoreFiles []string
var strengthen bool

var printHelp bool

func init() {
	flag.StringVarP(&destinationFile, "destination", "d", "", "(required) dhall output file")
	flag.StringVarP(&typeFile, "type", "t", "", "dhall output type file. ignored if useK8sSchema is not set")
	flag.DurationVar(&timeout, "timeout", 3*time.Minute, "length of time to run yaml-to-dhall command before timing out")
	flag.BoolVarP(&useK8sSchema, "useK8sSchema", "k", false, "use k8s schema for resource contents when generating output")
	flag.StringArrayVarP(&ignoreFiles, "ignore", "i", nil, "input files matching glob pattern will be ignored")
	flag.BoolVar(&strengthen, "strengthenSchema", false, "if set transforms output to stronger types (for example converts lists to records). ignored if useK8sSchema is set")
	flag.BoolVarP(&printHelp, "help", "h", false, "print usage instructions")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of ds-to-dhall:\n")
		flag.PrintDefaults()
	}
}

func main() {
	log15.Root().SetHandler(log15.StreamHandler(os.Stdout, log15.LogfmtFormat()))

	flag.Parse()

	if printHelp {
		flag.Usage()
		os.Exit(0)
	}

	if destinationFile == "" {
		flag.PrintDefaults()
		os.Exit(1)
	}

	inputs := flag.Args()
	if len(inputs) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			logFatal("failed to get cwd for sourceDirectory", "err", err)
		}
		inputs = []string{cwd}
	}

	log15.Info("loading resources", "inputs", inputs)
	srcSet, err := loadResourceSet(inputs)
	if err != nil {
		logFatal("failed to load source resources", "error", err, "inputs", inputs)
	}

	yamlBytes, err := buildYaml(buildRecord(srcSet))
	if err != nil {
		logFatal("failed to compose yaml", "error", err)
	}

	log15.Info("execute yaml-to-dhall", "destination", destinationFile)

	dhallType := ""
	if useK8sSchema {
		dhallType = composeK8sDhallType(srcSet)

	} else {
		dhallTypeStr, err := composeSimplifiedDhallType(yamlBytes)
		if err != nil {
			logFatal("failed to compose simplified dhall type", "error", err)
		}
		dhallType = dhallTypeStr
	}
	if typeFile != "" {
		err = ioutil.WriteFile(typeFile, []byte(dhallType), 0777)
		if err != nil {
			logFatal("failed to write dhall type", "error", err, "typeFile", typeFile)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err = yamlToDhall(ctx, dhallType, yamlBytes, destinationFile)
	if err != nil {
		_ = ioutil.WriteFile("record.yaml", yamlBytes, 0777)
		logFatal("failed to execute yaml-to-dhall", "error", err, "dhallType", dhallType, "yaml", "record.yaml")
	}

	log15.Info("done")
}

type Resource struct {
	Source     string
	Component  string
	Kind       string
	ApiVersion string
	Name       string
	DhallType  string
	Labels     map[string]string
	Contents   map[string]interface{}
}

type ResourceSet struct {
	Root       string
	Components map[string][]*Resource
}

func loadResource(rootDir string, filename string) (*Resource, error) {
	relPath, err := filepath.Rel(rootDir, filename)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br := bufio.NewReader(f)
	decoder := yaml.NewDecoder(br)

	var res Resource
	res.Source = filename
	err = decoder.Decode(&res.Contents)
	if err != nil {
		return nil, err
	}

	kind, ok := res.Contents["kind"].(string)
	if !ok {
		return nil, fmt.Errorf("resource %s is missing a kind field", filename)
	}
	res.Kind = kind

	apiVersion, ok := res.Contents["apiVersion"].(string)
	if !ok {
		return nil, fmt.Errorf("resource %s is missing a apiVersion field", filename)
	}
	res.ApiVersion = apiVersion

	res.DhallType = fmt.Sprintf("(https://raw.githubusercontent.com/dhall-lang/dhall-kubernetes/f4bf4b9ddf669f7149ec32150863a93d6c4b3ef1/1.18/schemas.dhall).%s.TokenType", res.Kind)

	metadata, ok := res.Contents["metadata"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resource %s is missing metadata", filename)
	}

	name, ok := metadata["name"].(string)
	if !ok {
		return nil, fmt.Errorf("resource %s is missing name field", filename)
	}
	res.Name = name

	labels, ok := metadata["labels"].(map[string]interface{})
	if !ok {
		// manifests without labels section exist
		labels = make(map[string]interface{})
	}

	componentLabel, ok := labels["app.kubernetes.io/component"].(string)
	if ok {
		res.Component = componentLabel
	} else {
		log15.Warn("deriving component from directory", "manifest", filename)
		res.Component = filepath.Dir(relPath)
		if res.Component == "." {
			res.Component = filepath.Base(rootDir)
		}
	}

	// patch statefulsets
	if res.Kind == "StatefulSet" {
		spec, ok := res.Contents["spec"].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("resource %s is missing spec section", filename)
		}
		volumeClaimTemplates, ok := spec["volumeClaimTemplates"].([]interface{})
		if !ok {
			return nil, fmt.Errorf("resource %s is missing volumeClaimTemplates section", filename)
		}
		for _, volumeClaimTemplate := range volumeClaimTemplates {
			vct, ok := volumeClaimTemplate.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resource %s is missing volumeClaimTemplate section", filename)
			}
			vct["apiVersion"] = "apps/v1"
			vct["kind"] = "PersistentVolumeClaim"
		}
	}

	return &res, err
}

func makeAbs(paths []string) ([]string, error) {
	var pas []string

	for _, path := range paths {
		pa, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		pas = append(pas, pa)
	}
	return pas, nil
}

func commonPrefix(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}

	cp := strings.Split(paths[0], string(os.PathSeparator))

	if len(cp) == 0 || (len(cp) == 1 && cp[0] == "") {
		return "/", nil
	}

	for _, path := range paths[1:] {
		ps := strings.Split(path, string(os.PathSeparator))
		if len(cp) > len(ps) {
			cp = cp[:len(ps)]
		}

		idx := 0
		for idx < len(cp) && cp[idx] == ps[idx] {
			idx++
		}
		cp = cp[:idx]
	}
	if len(cp) == 0 || (len(cp) == 1 && cp[0] == "") {
		return "/", nil
	}
	return strings.Join(cp, string(os.PathSeparator)), nil
}

// implements a primitive suffix match using filepath.Match
// note: these are not the same semantics as .gitignore
func matchIgnore(pattern, path string) (bool, error) {
	if len(path) == 0 {
		return false, nil
	}
	sep := string(os.PathSeparator)
	parts := strings.Split(path, sep)

	for idx := len(parts) - 1; idx >= 0; idx-- {
		p := strings.Join(parts[idx:], sep)

		ignore, err := filepath.Match(pattern, p)
		if err != nil {
			return false, err
		}
		if ignore {
			return true, nil
		}
	}
	return false, nil
}

func ignorePath(path string) (bool, error) {
	for _, ignorePattern := range ignoreFiles {
		ignore, err := matchIgnore(ignorePattern, path)
		if err != nil {
			return false, err
		}
		if ignore {
			return true, nil
		}
	}
	return false, nil
}

func loadResourceSet(inputs []string) (*ResourceSet, error) {
	pas, err := makeAbs(inputs)
	if err != nil {
		return nil, err
	}
	cr, err := commonPrefix(pas)
	if err != nil {
		return nil, err
	}
	var rs ResourceSet
	rs.Components = make(map[string][]*Resource)
	rs.Root = cr

	for _, input := range pas {
		err = filepath.Walk(input, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			ignore, err := ignorePath(path)
			if err != nil {
				return err
			}
			if ignore && info.IsDir() {
				return filepath.SkipDir
			}
			if ignore {
				return nil
			}
			if info.IsDir() {
				return nil
			}

			if filepath.Ext(path) == ".yaml" || filepath.Ext(path) == ".yml" {
				res, err := loadResource(rs.Root, path)
				if err != nil {
					return err
				}
				rs.Components[res.Component] = append(rs.Components[res.Component], res)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return &rs, nil
}

func composeK8sDhallType(rs *ResourceSet) string {
	var schemas []string

	for component, resources := range rs.Components {
		for _, r := range resources {
			s := fmt.Sprintf("{ %s : { %s : { %s : %s } } }", strings.Title(component), r.Kind, r.Name, r.DhallType)
			schemas = append(schemas, s)
		}
	}

	return strings.Join(schemas, " ⩓ ")
}

func composeSimplifiedDhallType(yamlBytes []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout*2)
	defer cancel()

	dhallTypeBytes, err := dhallRecordToType(ctx, yamlBytes)
	if err != nil {
		return "", err
	}

	rt, err := parseRecordType(bytes.NewReader(dhallTypeBytes))
	if err != nil {
		return "", err
	}

	transformRecordType(rt)

	var sb strings.Builder

	rt.ToDhall(&sb, 1)

	return sb.String(), nil
}

func strengthenRecord(rec map[string]interface{}) map[string]interface{} {
	if !strengthen || useK8sSchema {
		return rec
	}

	// more transformation will reveal themselves as we work through the PoC assignment
	srec, err := transformList2Record(rec)
	if err != nil {
		log15.Warn("failed to strengthen record, returning original", "record", rec, "err", err)
		return rec
	}
	srec = transformDockerImageSpec(srec)
	return srec
}

func buildRecord(rs *ResourceSet) map[string]interface{} {
	record := make(map[string]interface{})

	for component, resources := range rs.Components {
		compRec := make(map[string]map[string]interface{})
		record[strings.Title(component)] = compRec
		for _, r := range resources {
			kindRec := compRec[r.Kind]
			if kindRec == nil {
				kindRec = make(map[string]interface{})
				compRec[r.Kind] = kindRec
			}
			kindRec[r.Name] = strengthenRecord(r.Contents)
		}
	}

	return record
}

func buildYaml(dhallRecord map[string]interface{}) ([]byte, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)

	err := e.Encode(dhallRecord)
	if err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func yamlToDhall(ctx context.Context, schema string, yamlBytes []byte, dst string) error {
	var cmd *exec.Cmd
	if schema == "" {
		cmd = exec.CommandContext(ctx, "yaml-to-dhall", "--records-loose", "--output", dst)
	} else {
		cmd = exec.CommandContext(ctx, "yaml-to-dhall", schema, "--records-loose", "--output", dst)
	}
	cmd.Stdin = bytes.NewReader(yamlBytes)
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func yamlToSimplifiedDhall(ctx context.Context, yamlBytes []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "yaml-to-dhall", "--records-loose")
	cmd.Stdin = bytes.NewReader(yamlBytes)
	cmd.Stderr = os.Stderr
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func dhallRecordToType(ctx context.Context, yamlBytes []byte) ([]byte, error) {
	dhallRecordBytes, err := yamlToSimplifiedDhall(ctx, yamlBytes)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "dhall", "type")
	cmd.Stdin = bytes.NewReader(dhallRecordBytes)
	cmd.Stderr = os.Stderr
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err = cmd.Run()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil

}

func logFatal(message string, ctx ...interface{}) {
	log15.Error(message, ctx...)
	os.Exit(1)
}
