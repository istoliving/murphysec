package maven

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/murphysecurity/murphysec/logger"
	"github.com/murphysecurity/murphysec/utils"
	"github.com/pkg/errors"
	"github.com/vifraa/gopom"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var ErrMvnCmd = errors.New("Maven command exit with error")

type MvmCmdVersionInfo struct {
	MavenVer  string `json:"maven_ver"`
	JavaVer   string `json:"java_ver"`
	RawOutput string `json:"rawOutput"`
}

func checkMvnVersion() (*MvmCmdVersionInfo, error) {
	var mavenVersion string
	var javaVersion string
	data, e := exec.Command("mvn", "--version").Output()
	if e != nil {
		return nil, e
	}
	output := string(data)
	lines := strings.Split(output, "\n")
	versionPattern := regexp.MustCompile("Apache Maven (\\d+(?:\\.[0-9A-Za-z_-]+)+)")
	javaVersionPattern := regexp.MustCompile("Java version: (\\d+(?:\\.[0-9A-Za-z_-]+)*)")
	for _, it := range lines {
		line := strings.TrimSpace(it)
		if m := versionPattern.FindStringSubmatch(line); mavenVersion == "" && m != nil {
			mavenVersion = m[1]
			continue
		}
		if m := javaVersionPattern.FindStringSubmatch(line); javaVersion == "" && m != nil {
			javaVersion = m[1]
			continue
		}
	}

	return &MvmCmdVersionInfo{
		MavenVer:  mavenVersion,
		JavaVer:   javaVersion,
		RawOutput: output,
	}, nil
}

func listPomProfiles(ctx context.Context, pomPath string) (profiles []string) {
	project, e := gopom.Parse(pomPath)
	if e != nil || project == nil {
		return
	}
	for _, profile := range project.Profiles {
		profiles = append(profiles, profile.ID)
	}
	return
}

func scanMvnDependency(ctx context.Context, projectDir string) (map[Coordinate][]Dependency, error) {
	if ctx == nil {
		ctx = context.TODO()
	}

	// handle maven profiles
	var profiles = listPomProfiles(ctx, filepath.Join(projectDir, "pom.xml"))
	var args = []string{"com.github.ferstl:depgraph-maven-plugin:4.0.1:graph", "-DgraphFormat=json", "--batch-mode"}
	if len(profiles) > 0 {
		args = append(args, "-P")
		args = append(args, strings.Join(profiles, ","))
	}
	c := exec.CommandContext(ctx, "mvn", args...)
	c.Dir = projectDir
	logger.Info.Println("Command:", c.String())
	logStream := utils.NewLogPipe(logger.Info.Logger) // todo: refactor logger
	defer logStream.Close()
	cmdErr := &mvnCmdErr{errOutput: NewMvnCmdExecution()}
	c.Stderr = io.MultiWriter(cmdErr.errOutput, logStream)
	c.Stdout = io.MultiWriter(cmdErr.errOutput, logStream)
	if e := c.Start(); e != nil {
		return nil, errors.Wrap(e, "Mvn command execute failed")
	}
	if e := c.Wait(); e != nil {
		cmdErr.code = c.ProcessState.ExitCode()
		cmdErr.err = e
		logger.Err.Println("mvn exit with error")
		return nil, cmdErr
	}
	logger.Info.Println("mvn exit with no errors")
	logger.Info.Println("Walk dir collect dependency-graph.json")
	var graphPaths []string
	filepath.Walk(projectDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.Name() == "dependency-graph.json" {
			graphPaths = append(graphPaths, path)
		}
		return nil
	})
	logger.Info.Println("Total", len(graphPaths), "graphs")
	rmap := map[Coordinate][]Dependency{}
	for _, p := range graphPaths {
		coor := readCoordinate(filepath.Dir(filepath.Dir(p)))
		if coor == nil {
			continue
		}
		data, e := os.ReadFile(p)
		if e != nil {
			logger.Warn.Println("Read graph failed.", p, e.Error())
			continue
		}
		var g dependencyGraph
		if e := json.Unmarshal(data, &g); e != nil {
			logger.Warn.Println("Parse graph failed.", p, e.Error())
			continue
		}
		rmap[*coor] = g.Tree()
	}
	return rmap, nil
}

type dependencyGraph struct {
	GraphName string `json:"graphName"`
	Artifacts []struct {
		GroupId    string   `json:"groupId"`
		ArtifactId string   `json:"artifactId"`
		Optional   bool     `json:"optional"`
		Scopes     []string `json:"scopes"`
		Version    string   `json:"version"`
	} `json:"artifacts"`
	Dependencies []struct {
		NumericFrom int `json:"numericFrom"`
		NumericTo   int `json:"numericTo"`
	} `json:"dependencies"`
}

func (d dependencyGraph) Tree() []Dependency {
	// from -> listOf to
	edges := map[int][]int{}
	for _, it := range d.Dependencies {
		edges[it.NumericFrom] = append(edges[it.NumericFrom], it.NumericTo)
	}

	root := make([]bool, len(d.Artifacts))
	for _, it := range d.Dependencies {
		root[it.NumericTo] = true
	}
	var rootNums []int
	for idx, it := range root {
		if !it {
			rootNums = append(rootNums, idx)
		}
	}
	if len(rootNums) == 0 {
		logger.Warn.Println("root node not found")
		return nil
	} else if len(rootNums) > 1 {
		logger.Warn.Println("found more than one root node")
		visited := make([]bool, len(d.Artifacts))
		var rs []Dependency
		for _, rootN := range rootNums {
			t := d._tree(rootN, visited, edges)
			if t == nil {
				continue
			}
			rs = append(rs, *t)
		}
		return rs
	} else {
		visited := make([]bool, len(d.Artifacts))
		t := d._tree(rootNums[0], visited, edges)
		if t != nil {
			return t.Children
		}
	}
	visited := make([]bool, len(d.Artifacts))
	t := d._tree(rootNums[0], visited, edges)
	if t != nil {
		return t.Children
	}
	return nil
}

func (d dependencyGraph) _tree(id int, visitedId []bool, edges map[int][]int) *Dependency {
	if visitedId[id] {
		return nil
	}
	visitedId[id] = true
	defer func() { visitedId[id] = false }()

	if !utils.InStringSlice(d.Artifacts[id].Scopes, "compile") && !utils.InStringSlice(d.Artifacts[id].Scopes, "runtime") {
		return nil
	}
	r := &Dependency{
		Coordinate: Coordinate{
			GroupId:    d.Artifacts[id].GroupId,
			ArtifactId: d.Artifacts[id].ArtifactId,
			Version:    d.Artifacts[id].Version,
		},
		Children: nil,
	}
	for _, toNum := range edges[id] {
		t := d._tree(toNum, visitedId, edges)
		if t == nil {
			continue
		}
		r.Children = append(r.Children, *t)
	}
	return r
}

type mvnCmdErr struct {
	code      int
	err       error
	errOutput *MvnCmdExecutionStreamHandler
}

func (e mvnCmdErr) Error() string {
	if e.err != nil {
		return fmt.Sprintf("Mvn command error[%d]: %s. Output: %s", e.code, e.err.Error(), e.errOutput)
	}
	return fmt.Sprintf("Mvn command error[%d], Output: %s", e.code, e.errOutput)
}

func (e mvnCmdErr) Unwrap() error {
	return e.err
}

func (e mvnCmdErr) Is(target error) bool {
	return target == ErrMvnCmd
}

func readCoordinate(dir string) *Coordinate {
	data, e := os.ReadFile(filepath.Join(dir, "pom.xml"))
	if e != nil {
		return nil
	}
	var p gopom.Project
	if e := xml.Unmarshal(data, &p); e != nil {
		return nil
	}
	c := &Coordinate{
		GroupId:    p.GroupID,
		ArtifactId: p.ArtifactID,
		Version:    p.Version,
	}
	if c.GroupId == "" {
		c.GroupId = p.Parent.GroupID
	}
	if c.ArtifactId == "" {
		c.ArtifactId = p.Parent.ArtifactID
	}
	if c.Version == "" {
		c.Version = p.Parent.Version
	}
	return c
}

// checkMvnEnv 检查maven环境
func checkMvnEnv() (bool, *MvmCmdVersionInfo) {
	if os.Getenv("NO_MVN") != "" {
		logger.Err.Println("NO_MVN environment found. Skip maven scan")
		return false, nil
	}
	ver, e := checkMvnVersion()
	if e != nil {
		logger.Err.Println("Get mvn command version failed, skip maven scan.")
		return false, nil
	}
	logger.Info.Println("Mvn command version:", ver.MavenVer)
	return true, ver
}
